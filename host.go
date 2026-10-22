package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofrs/flock"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type Host struct {
	dir       string
	cfg       Config
	mu        sync.Mutex
	servers   map[string]*Managed
	playit    *Playit
	stats     Stats
	installMu sync.Mutex
}

func newHost(dir string, cfg Config) (*Host, error) {
	if e := os.MkdirAll(filepath.Join(dir, "servers"), 0700); e != nil {
		return nil, e
	}
	h := &Host{dir: dir, cfg: cfg, servers: map[string]*Managed{}}
	h.playit = newPlayit(filepath.Join(dir, "playit"))
	entries, _ := os.ReadDir(filepath.Join(dir, "servers"))
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		var s Server
		root := filepath.Join(dir, "servers", en.Name())
		if readJSON(filepath.Join(root, "server.json"), &s) != nil {
			continue
		}
		m := newManaged(root, s)
		h.servers[s.ID] = m
		m.recoverProcess()
		go m.followLogs()
		if s.AutoStart && m.snapshot().PID == 0 {
			go func() {
				time.Sleep(3 * time.Second)
				if _, e := os.Stat(filepath.Join(m.root, "installed.json")); e == nil {
					m.start()
				}
			}()
		}
	}
	return h, nil
}

// idle reports whether no game is running or being installed, backed up or restored,
// so restarting the agent interrupts nothing.
func (h *Host) idle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.servers {
		if m.snapshot().PID != 0 || !m.op.TryLock() {
			return false
		}
		m.op.Unlock()
	}
	return true
}
func (h *Host) heartbeat() Wire {
	h.mu.Lock()
	states := []Runtime{}
	for _, s := range h.servers {
		states = append(states, s.snapshot())
	}
	stats := h.stats
	h.mu.Unlock()
	host, _ := os.Hostname()
	if h.cfg.Name != "" {
		host = h.cfg.Name
	}
	p := h.playit.snapshot()
	return Wire{Type: "heartbeat", Node: &Node{ID: h.cfg.AgentID, Name: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Stats: stats}, Servers: states, Playit: &p}
}
func (h *Host) metrics(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		var s Stats
		if v, e := cpu.Percent(0, false); e == nil && len(v) > 0 {
			s.CPU = v[0]
		}
		if v, e := mem.VirtualMemory(); e == nil {
			s.RAMTotal = v.Total
			s.RAMUsed = v.Used
		}
		if v, e := disk.Usage(h.dir); e == nil {
			s.DiskFree = v.Free
			s.DiskTotal = v.Total
		}
		h.mu.Lock()
		h.stats = s
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (h *Host) handle(cmd Command) (any, error) {
	switch cmd.Action {
	case "playit.connect":
		return h.playit.claim()
	case "playit.refresh":
		return h.playit.refresh()
	case "playit.start":
		return map[string]bool{"ok": true}, h.playit.start()
	case "playit.stop":
		return map[string]bool{"ok": true}, h.playit.stop()
	}
	id := cmd.ServerID
	if cmd.Server != nil {
		id = cmd.Server.ID
	}
	if !safeID.MatchString(id) {
		return nil, errors.New("invalid server ID")
	}
	h.mu.Lock()
	m := h.servers[id]
	h.mu.Unlock()
	if cmd.Action == "install" {
		if cmd.Server == nil {
			return nil, errors.New("missing installation plan")
		}
		if m == nil {
			m = newManaged(filepath.Join(h.dir, "servers", id), *cmd.Server)
			if e := os.MkdirAll(m.root, 0700); e != nil {
				return nil, e
			}
			h.mu.Lock()
			h.servers[id] = m
			h.mu.Unlock()
			go m.followLogs()
		}
		if !m.op.TryLock() {
			return nil, errors.New("server is busy")
		}
		if m.snapshot().PID != 0 {
			m.op.Unlock()
			return nil, errors.New("stop the server before installing or updating")
		}
		m.mu.Lock()
		m.spec = *cmd.Server
		m.state.State = "installing"
		m.state.Message = "Waiting for installer"
		m.mu.Unlock()
		if e := writeJSON(filepath.Join(m.root, "server.json"), m.specCopy()); e != nil {
			m.op.Unlock()
			return nil, e
		}
		go func() {
			defer m.op.Unlock()
			h.installMu.Lock()
			defer h.installMu.Unlock()
			m.log("Starting installation")
			e := installGame(h, m)
			if e != nil {
				m.setState("error", e.Error())
				m.log("Installation failed: " + e.Error())
				return
			}
			if e = writeJSON(filepath.Join(m.root, "server.json"), m.specCopy()); e == nil {
				e = writeJSON(filepath.Join(m.root, "installed.json"), map[string]any{"time": time.Now(), "version": Version})
			}
			if e != nil {
				m.setState("error", "Could not save installation state: "+e.Error())
				return
			}
			m.setState("stopped", "Installed. Ready to start.")
			m.log("Installation complete. Click Start to launch your server.")
		}()
		return map[string]string{"message": "Installation queued"}, nil
	}
	if m == nil {
		return nil, errors.New("server is not installed on this computer; choose Install / repair")
	}
	switch cmd.Action {
	case "start":
		return map[string]bool{"ok": true}, m.start()
	case "stop":
		return map[string]bool{"ok": true}, m.stop()
	case "restart":
		if e := m.stop(); e != nil {
			return nil, e
		}
		return map[string]bool{"ok": true}, m.start()
	case "console":
		return map[string]bool{"ok": true}, m.console(cmd.Args["command"])
	case "files":
		return m.files(cmd.Args["path"])
	case "read":
		return m.readFile(cmd.Args["path"])
	case "write", "upload":
		return map[string]bool{"ok": true}, m.writeFile(cmd.Args, cmd.Action == "upload")
	case "backups":
		return m.backups()
	case "backup":
		return m.backup()
	case "restore":
		return map[string]bool{"ok": true}, m.restore(cmd.Args["name"])
	case "tunnel":
		return h.playit.tunnel(m.specCopy())
	case "settings":
		if !m.op.TryLock() {
			return nil, errors.New("server is busy")
		}
		defer m.op.Unlock()
		if m.snapshot().PID != 0 {
			return nil, errors.New("stop the server before changing settings")
		}
		if cmd.Server == nil {
			return nil, errors.New("missing settings")
		}
		old := m.specCopy()
		// The installed executable/runtime paths belong to this host, not the controller's draft plan.
		incoming := clone(*cmd.Server)
		incoming.Plan = old.Plan
		if v := old.Settings["installedVersion"]; v != "" {
			incoming.Settings["installedVersion"] = v
		}
		m.mu.Lock()
		m.spec = incoming
		m.mu.Unlock()
		if e := configureGame(m); e != nil {
			m.mu.Lock()
			m.spec = old
			m.mu.Unlock()
			return nil, e
		}
		return map[string]bool{"ok": true}, writeJSON(filepath.Join(m.root, "server.json"), m.specCopy())
	}
	return nil, errors.New("unknown host action")
}
func runHost(ctx context.Context, dir string, cfg Config) error {
	os.MkdirAll(dir, 0700)
	lock := flock.New(filepath.Join(dir, "host.lock"))
	ok, e := lock.TryLock()
	if e != nil {
		return e
	}
	if !ok {
		return errors.New("LANForge host is already running")
	}
	defer lock.Unlock()
	h, e := newHost(dir, cfg)
	if e != nil {
		return e
	}
	go h.metrics(ctx)
	go updateLoop(ctx, h.idle)
	go h.playit.loop(ctx)
	go h.autoTunnels(ctx)
	// Results are retained for the lifetime of the agent to avoid repeat actions on reconnect.
	seen := map[string]Wire{}
	var seenMu sync.Mutex
	busy := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return nil
		}
		u, e := url.Parse(cfg.Controller)
		if e != nil {
			return e
		}
		u.Scheme = "wss"
		u.Path = "/api/agent/connect"
		q := u.Query()
		q.Set("id", cfg.AgentID)
		u.RawQuery = q.Encode()
		tlsCfg, e := pinnedTLS(cfg.Fingerprint)
		if e != nil {
			return e
		}
		dial := websocket.Dialer{TLSClientConfig: tlsCfg, HandshakeTimeout: 10 * time.Second}
		ws, _, e := dial.DialContext(ctx, u.String(), http.Header{"Authorization": []string{"Bearer " + cfg.Token}})
		if e != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(4 * time.Second):
				continue
			}
		}
		send := make(chan Wire, 32)
		done := make(chan struct{})
		var writeMu sync.Mutex
		go func() {
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				var msg Wire
				select {
				case <-done:
					return
				case <-ctx.Done():
					ws.Close()
					return
				case msg = <-send:
				case <-t.C:
					msg = h.heartbeat()
				}
				writeMu.Lock()
				ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
				e := ws.WriteJSON(msg)
				writeMu.Unlock()
				if e != nil {
					ws.Close()
					return
				}
			}
		}()
		ws.SetReadLimit(16 << 20)
		ws.SetReadDeadline(time.Now().Add(40 * time.Second))
		ws.SetPingHandler(func(s string) error {
			ws.SetReadDeadline(time.Now().Add(40 * time.Second))
			return ws.WriteControl(websocket.PongMessage, []byte(s), time.Now().Add(5*time.Second))
		})
		send <- h.heartbeat()
		for {
			var in Wire
			if e := ws.ReadJSON(&in); e != nil {
				break
			}
			ws.SetReadDeadline(time.Now().Add(40 * time.Second))
			if in.Type != "command" || in.Command == nil {
				continue
			}
			cmd := *in.Command
			seenMu.Lock()
			old, exists := seen[cmd.ID]
			running := busy[cmd.ID]
			if !exists && !running {
				busy[cmd.ID] = true
			}
			seenMu.Unlock()
			if exists {
				select {
				case send <- old:
				case <-done:
				}
				continue
			}
			if running {
				continue
			}
			go func() {
				res, err := h.handle(cmd)
				out := Wire{Type: "result", ID: cmd.ID, Data: res}
				if err != nil {
					out.Error = err.Error()
				}
				seenMu.Lock()
				seen[cmd.ID] = out
				delete(busy, cmd.ID)
				if len(seen) > 1000 {
					for k := range seen {
						if k != cmd.ID {
							delete(seen, k)
							break
						}
					}
				}
				seenMu.Unlock()
				select {
				case send <- out:
				case <-done:
				case <-ctx.Done():
				}
			}()
		}
		close(done)
		ws.Close()
	}
}
func pairHost(ctx context.Context, controller, fp, code, name string) (Config, error) {
	cfg := Config{Role: "host", Name: name, Controller: controller, Fingerprint: fp}
	u, e := url.Parse(controller)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return cfg, errors.New("controller address must look like https://192.168.1.10:8443")
	}
	u.Path = ""
	u.Fragment = ""
	cfg.Controller = u.String()
	tc, e := pinnedTLS(fp)
	if e != nil {
		return cfg, e
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 15 * time.Second}
	b, _ := json.Marshal(map[string]string{"code": code, "name": name, "os": runtime.GOOS, "arch": runtime.GOARCH})
	req, e := http.NewRequestWithContext(ctx, "POST", cfg.Controller+"/api/pair", bytesReader(b))
	if e != nil {
		return cfg, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LanForge", "1")
	res, e := client.Do(req)
	if e != nil {
		return cfg, fmt.Errorf("could not reach controller at %s: %w. Check that this is the main PC's Wi-Fi/Ethernet address (run ipconfig there; 192.168.56.x is VirtualBox and unreachable), both PCs are on the same network, and the controller's firewall allows TCP %s on Private networks", u.Host, e, u.Port())
	}
	defer res.Body.Close()
	var out map[string]string
	if e = json.NewDecoder(res.Body).Decode(&out); e != nil {
		return cfg, e
	}
	if res.StatusCode != 200 {
		return cfg, errors.New(out["error"])
	}
	cfg.AgentID = out["id"]
	cfg.Token = out["token"]
	return cfg, nil
}

func (h *Host) autoTunnels(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		h.playit.mu.Lock()
		linked := h.playit.secret != "" && h.playit.enabled
		keyID := hashToken(h.playit.secret)
		h.playit.mu.Unlock()
		if !linked {
			continue
		}
		h.mu.Lock()
		list := make([]*Managed, 0, len(h.servers))
		for _, m := range h.servers {
			list = append(list, m)
		}
		h.mu.Unlock()
		for _, m := range list {
			s := m.specCopy()
			if s.Mode != "playit" {
				continue
			}
			if _, e := os.Stat(filepath.Join(m.root, "installed.json")); e != nil {
				continue
			}
			if m.snapshot().State == "installing" {
				continue
			}
			marker := filepath.Join(m.root, "tunnel-attempt.json")
			var prev string
			if readJSON(marker, &prev) == nil && prev == keyID {
				continue
			}
			// Attempt once per account. Permission/plan errors need user action, never a retry loop.
			writeJSON(marker, keyID)
			if _, e := h.playit.tunnel(s); e != nil {
				m.log("Playit setup needs attention: " + e.Error())
			} else {
				m.log("Playit tunnel configured. Open Connect for the public address.")
			}
		}
	}
}
