package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/shirou/gopsutil/v4/process"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var regexpWorld = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)

type Managed struct {
	mu           sync.Mutex
	op           sync.Mutex
	root         string
	spec         Server
	state        Runtime
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	done         chan struct{}
	intentional  bool
	crashes      int
	recovered    bool
	launchOffset int64
}
type processIdentity struct {
	PID     int   `json:"pid"`
	Created int64 `json:"created"`
}

func newManaged(root string, s Server) *Managed {
	return &Managed{root: root, spec: s, state: Runtime{ID: s.ID, State: "stopped", Message: "Ready to start", Logs: []string{}}}
}
func (m *Managed) specCopy() Server { m.mu.Lock(); defer m.mu.Unlock(); return clone(m.spec) }
func (m *Managed) setState(state, msg string) {
	m.mu.Lock()
	m.state.State = state
	m.state.Message = msg
	m.mu.Unlock()
}
func (m *Managed) log(s string) { m.mu.Lock(); defer m.mu.Unlock(); m.appendLog("[LANForge] " + s) }
func (m *Managed) appendLog(s string) {
	if len(s) > 8192 {
		s = s[:8192] + "…"
	}
	m.state.Logs = append(m.state.Logs, s)
	if len(m.state.Logs) > 250 {
		m.state.Logs = m.state.Logs[len(m.state.Logs)-250:]
	}
}
func (m *Managed) snapshot() Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.Logs = append([]string{}, m.state.Logs...)
	if s.PID > 0 {
		if p, e := process.NewProcess(int32(s.PID)); e == nil {
			if v, e := p.MemoryInfo(); e == nil {
				s.RAM = v.RSS
			}
			if v, e := p.CPUPercent(); e == nil {
				s.CPU = v
			}
		}
	}
	return s
}
func (m *Managed) recoverProcess() {
	var id processIdentity
	if readJSON(filepath.Join(m.root, "process.json"), &id) != nil {
		return
	}
	p, e := process.NewProcess(int32(id.PID))
	if e != nil {
		return
	}
	birth, e := p.CreateTime()
	if e != nil || birth != id.Created {
		return
	}
	m.mu.Lock()
	m.state.PID = id.PID
	m.state.State = "running"
	m.state.Message = "Recovered running process. Restart to reconnect console input."
	m.recovered = true
	m.done = make(chan struct{})
	m.mu.Unlock()
	go func() {
		for {
			running, e := p.IsRunning()
			birth, _ := p.CreateTime()
			if e != nil || !running || birth != id.Created {
				m.exited(errors.New("recovered process exited"))
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()
}
func (m *Managed) followLogs() {
	var offset int64
	path := filepath.Join(m.root, "console.log")
	for {
		time.Sleep(time.Second)
		f, e := os.Open(path)
		if e != nil {
			continue
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			continue
		}
		if st.Size() < offset {
			offset = 0
		}
		if offset == 0 && st.Size() > 256<<10 {
			offset = st.Size() - (256 << 10)
		}
		f.Seek(offset, io.SeekStart)
		reader := bufio.NewReader(io.LimitReader(f, 256<<10))
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				offset += int64(len(line))
				line = strings.TrimSpace(line)
				if line != "" {
					m.mu.Lock()
					m.appendLog(line)
					if m.state.PID > 0 && m.state.State == "starting" && offset > m.launchOffset && m.isReady(line) {
						m.state.State = "running"
						m.state.Message = "Server is ready"
					}
					m.mu.Unlock()
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
		m.mu.Lock()
		if m.state.State == "starting" && time.Since(m.state.Started) > 5*time.Minute {
			m.state.Message = "Process is running; waiting for game readiness. Check console."
		}
		m.mu.Unlock()
	}
}
func (m *Managed) isReady(line string) bool {
	switch m.spec.Engine {
	case "minecraft":
		return strings.Contains(line, "Done (") && strings.Contains(line, "For help")
	case "terraria":
		return strings.Contains(line, "Listening on port")
	case "valheim":
		return strings.Contains(line, "Game server connected")
	}
	return false
}
func (m *Managed) checkPorts() error {
	s := m.specCopy()
	for _, p := range s.Ports {
		addr := fmt.Sprintf("0.0.0.0:%d", p.Number)
		if p.Protocol == "tcp" || p.Protocol == "both" {
			// Windows lets 0.0.0.0 bind over a loopback listener, so check both.
			for _, a := range []string{addr, fmt.Sprintf("127.0.0.1:%d", p.Number)} {
				l, e := net.Listen("tcp4", a)
				if e != nil {
					return fmt.Errorf("TCP port %d is in use by another program", p.Number)
				}
				l.Close()
			}
		}
		if p.Protocol == "udp" || p.Protocol == "both" {
			l, e := net.ListenPacket("udp4", addr)
			if e != nil {
				return fmt.Errorf("UDP port %d is in use by another program", p.Number)
			}
			l.Close()
		}
	}
	return nil
}
func (m *Managed) start() error {
	if !m.op.TryLock() {
		return errors.New("server is busy")
	}
	defer m.op.Unlock()
	if m.snapshot().PID != 0 {
		return nil
	}
	if _, e := os.Stat(filepath.Join(m.root, "installed.json")); e != nil {
		return errors.New("install the server first")
	}
	if e := m.checkPorts(); e != nil {
		return e
	}
	s := m.specCopy()
	if s.Plan == nil {
		return errors.New("installation plan is missing")
	}
	p := s.Plan
	if e := runSteps(nil, m, p.PreStart); e != nil {
		return e
	}
	exe, e := inside(m.root, p.Executable)
	if e != nil {
		return e
	}
	work, e := inside(m.root, p.WorkDir)
	if e != nil {
		return e
	}
	if _, e = os.Stat(exe); e != nil {
		return fmt.Errorf("server executable missing: %s; use Install / repair", p.Executable)
	}
	os.MkdirAll(work, 0700)
	logPath := filepath.Join(m.root, "console.log")
	if st, e := os.Stat(logPath); e == nil && st.Size() > 8<<20 {
		os.Rename(logPath, filepath.Join(m.root, "console.previous.log"))
	}
	f, e := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if info, err := f.Stat(); err == nil {
		m.mu.Lock()
		m.launchOffset = info.Size()
		m.mu.Unlock()
	}
	argv := make([]string, len(p.Args))
	for i, a := range p.Args {
		argv[i] = strings.ReplaceAll(a, "$ROOT", m.root)
	}
	cmd := exec.Command(exe, argv...)
	cmd.Dir = work
	cmd.Env = os.Environ()
	for k, v := range p.Env {
		cmd.Env = append(cmd.Env, k+"="+envVariableRE.ReplaceAllStringFunc(strings.ReplaceAll(v, "$ROOT", m.root), func(x string) string { return os.Getenv(x[1 : len(x)-1]) }))
	}
	cmd.Stdout = f
	cmd.Stderr = f
	prepareProcess(cmd)
	stdin, e := cmd.StdinPipe()
	if e != nil {
		f.Close()
		return e
	}
	if e = cmd.Start(); e != nil {
		stdin.Close()
		f.Close()
		return fmt.Errorf("could not start %s: %w", filepath.Base(exe), e)
	}
	f.Close()
	proc, _ := process.NewProcess(int32(cmd.Process.Pid))
	var birth int64
	if proc != nil {
		birth, _ = proc.CreateTime()
	}
	writeJSON(filepath.Join(m.root, "process.json"), processIdentity{cmd.Process.Pid, birth})
	m.mu.Lock()
	m.cmd = cmd
	m.stdin = stdin
	m.done = make(chan struct{})
	m.intentional = false
	m.recovered = false
	m.state.PID = cmd.Process.Pid
	m.state.Started = time.Now()
	m.state.State = "starting"
	m.state.Message = "Starting game server"
	if s.Engine == "amp" {
		m.state.State = "running"
		m.state.Message = "Process running; game readiness is not verified"
	}
	m.mu.Unlock()
	m.log("Server process started")
	go func() { err := cmd.Wait(); m.exited(err) }()
	return nil
}
func (m *Managed) exited(err error) {
	m.mu.Lock()
	intentional := m.intentional
	started := m.state.Started
	auto := m.spec.RestartOnCrash
	m.state.PID = 0
	m.cmd = nil
	if m.stdin != nil {
		m.stdin.Close()
		m.stdin = nil
	}
	m.state.State = "stopped"
	m.state.Message = "Server stopped"
	if !intentional && err != nil {
		m.state.State = "error"
		m.state.Message = "Server exited: " + err.Error()
	}
	os.Remove(filepath.Join(m.root, "process.json"))
	if m.done != nil {
		close(m.done)
		m.done = nil
	}
	if time.Since(started) > 10*time.Minute {
		m.crashes = 0
	}
	retry := !intentional && err != nil && auto && m.crashes < 3
	if retry {
		m.crashes++
		m.state.Message = "Server crashed; restarting in 10 seconds"
	}
	m.mu.Unlock()
	if retry {
		go func() {
			time.Sleep(10 * time.Second)
			m.mu.Lock()
			cancelled := m.intentional
			m.mu.Unlock()
			if !cancelled {
				m.start()
			}
		}()
	}
}
func (m *Managed) stop() error {
	if !m.op.TryLock() {
		return errors.New("server is busy")
	}
	defer m.op.Unlock()
	m.mu.Lock()
	m.intentional = true
	if m.state.PID == 0 {
		m.mu.Unlock()
		return nil
	}
	cmd := m.cmd
	pid := m.state.PID
	stdin := m.stdin
	stop := ""
	if m.spec.Plan != nil {
		stop = m.spec.Plan.Stop
	}
	done := m.done
	m.state.State = "stopping"
	m.state.Message = "Saving and stopping the server"
	m.mu.Unlock()
	if cmd == nil {
		p, e := os.FindProcess(pid)
		if e != nil {
			return e
		}
		cmd = &exec.Cmd{Process: p}
	}
	if stdin != nil && stop != "" {
		io.WriteString(stdin, stop+"\n")
	} else {
		stopProcess(cmd, false)
	}
	select {
	case <-done:
		return nil
	case <-time.After(35 * time.Second):
		m.log("Graceful stop timed out. Terminating remaining process tree.")
		stopProcess(cmd, true)
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("process did not stop; check the host computer")
		}
	}
}
func (m *Managed) console(line string) error {
	if len(line) > 4096 || strings.ContainsAny(line, "\r\n\x00") {
		return errors.New("enter one console command at a time")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stdin == nil {
		return errors.New("console input is unavailable; the server must be running and managed by this host session")
	}
	_, e := io.WriteString(m.stdin, line+"\n")
	return e
}
func (m *Managed) files(rel string) (any, error) {
	p, e := inside(filepath.Join(m.root, "game"), rel)
	if e != nil {
		return nil, e
	}
	es, e := os.ReadDir(p)
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, f := range es {
		if f.Type()&os.ModeSymlink != 0 {
			continue
		}
		st, e := f.Info()
		if e != nil {
			continue
		}
		out = append(out, map[string]any{"name": f.Name(), "directory": f.IsDir(), "size": st.Size(), "modified": st.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a["directory"] != b["directory"] {
			return a["directory"].(bool)
		}
		return a["name"].(string) < b["name"].(string)
	})
	return out, nil
}
func (m *Managed) readFile(rel string) (any, error) {
	p, e := inside(filepath.Join(m.root, "game"), rel)
	if e != nil {
		return nil, e
	}
	st, e := os.Stat(p)
	if e != nil {
		return nil, e
	}
	if st.Size() > 2<<20 {
		return nil, errors.New("editor supports files up to 2 MB; open larger files on the host")
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	if strings.IndexByte(string(b), 0) >= 0 {
		return nil, errors.New("this is a binary file; use the host's file manager")
	}
	return map[string]string{"content": string(b), "path": rel}, nil
}
func (m *Managed) writeFile(args map[string]string, binary bool) error {
	if !m.op.TryLock() {
		return errors.New("server is busy")
	}
	defer m.op.Unlock()
	if m.snapshot().PID != 0 {
		return errors.New("stop the server before changing its files")
	}
	p, e := inside(filepath.Join(m.root, "game"), args["path"])
	if e != nil {
		return e
	}
	if args["path"] == "" {
		return errors.New("choose a file")
	}
	b := []byte(args["content"])
	if binary {
		b, e = base64.StdEncoding.DecodeString(args["content"])
		if e != nil {
			return e
		}
	}
	if len(b) > 8<<20 {
		return errors.New("uploads are limited to 8 MB; copy larger files directly on the host")
	}
	if old, e := os.ReadFile(p); e == nil {
		if e = atomicWrite(p+".lanforge-backup", old, 0600); e != nil {
			return e
		}
	}
	return atomicWrite(p, b, 0600)
}
func fmtPID(pid int) string { return strconv.Itoa(pid) }
