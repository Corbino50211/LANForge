package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shirou/gopsutil/v4/process"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Playit struct {
	mu             sync.Mutex
	op             sync.Mutex
	dir            string
	secret         string
	state          PlayitState
	cmd            *exec.Cmd
	recovered      *process.Process
	recoveredBirth int64
	claiming       bool
	enabled        bool
}

func newPlayit(dir string) *Playit {
	os.MkdirAll(dir, 0700)
	p := &Playit{dir: dir, state: PlayitState{State: "disconnected", Message: "Connect Playit to share servers without port forwarding", Tunnels: []map[string]any{}}}
	if b, e := os.ReadFile(filepath.Join(dir, "secret")); e == nil {
		p.secret = strings.TrimSpace(string(b))
		p.state.State = "linked"
		p.state.Message = "Account linked"
	}
	readJSON(filepath.Join(dir, "enabled.json"), &p.enabled)
	var id processIdentity
	if readJSON(filepath.Join(dir, "process.json"), &id) == nil {
		if proc, err := process.NewProcess(int32(id.PID)); err == nil {
			if birth, err := proc.CreateTime(); err == nil && birth == id.Created {
				p.recovered = proc
				p.recoveredBirth = birth
				p.state.State = "running"
				p.state.Message = "Recovered Playit agent"
			}
		}
	}
	return p
}
func (p *Playit) snapshot() PlayitState {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Linked = p.secret != ""
	return clone(p.state)
}
func (p *Playit) update(state, msg string) {
	p.mu.Lock()
	p.state.State = state
	p.state.Message = msg
	p.mu.Unlock()
}
func playitAPI(path, key string, body any) (json.RawMessage, error) {
	b, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequest("POST", "https://api.playit.gg"+path, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "LanForge/"+Version)
	if key != "" {
		req.Header.Set("Authorization", "Agent-Key "+key)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&env); e != nil {
		return nil, fmt.Errorf("Playit returned HTTP %d", res.StatusCode)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("Playit: %s", string(env.Data))
	}
	return env.Data, nil
}
func (p *Playit) claim() (any, error) {
	p.mu.Lock()
	if p.secret != "" {
		p.mu.Unlock()
		return p.snapshot(), nil
	}
	if p.claiming {
		out := p.state
		p.mu.Unlock()
		return out, nil
	}
	p.claiming = true
	code := token(5)
	p.state.State = "claiming"
	p.state.ClaimURL = "https://playit.gg/claim/" + code
	p.state.Message = "Open the link and approve LANForge in your Playit account"
	out := p.state
	p.mu.Unlock()
	go func() {
		defer func() { p.mu.Lock(); p.claiming = false; p.mu.Unlock() }()
		deadline := time.Now().Add(20 * time.Minute)
		for time.Now().Before(deadline) {
			data, e := playitAPI("/claim/setup", "", map[string]string{"code": code, "agent_type": "self-managed", "version": "LanForge " + Version})
			if e != nil {
				p.update("error", e.Error()+". Click Connect to try again.")
				return
			}
			var state string
			json.Unmarshal(data, &state)
			switch state {
			case "UserRejected":
				p.update("disconnected", "Playit connection was declined")
				return
			case "UserAccepted":
				data, e = playitAPI("/claim/exchange", "", map[string]string{"code": code})
				if e != nil {
					p.update("error", e.Error())
					return
				}
				var v struct {
					Secret string `json:"secret_key"`
				}
				if json.Unmarshal(data, &v) != nil || v.Secret == "" {
					p.update("error", "Playit did not return an agent key")
					return
				}
				if e = atomicWrite(filepath.Join(p.dir, "secret"), []byte(v.Secret), 0600); e != nil {
					p.update("error", e.Error())
					return
				}
				p.mu.Lock()
				p.secret = v.Secret
				p.state.ClaimURL = ""
				p.mu.Unlock()
				if e = p.start(); e != nil {
					p.update("error", e.Error())
				}
				return
			}
			time.Sleep(2 * time.Second)
		}
		p.update("disconnected", "Claim link expired. Click Connect for a new link.")
	}()
	return out, nil
}
func (p *Playit) start() error {
	if !p.op.TryLock() {
		return errors.New("Playit is already starting or busy")
	}
	defer p.op.Unlock()
	p.mu.Lock()
	if p.cmd != nil || p.recovered != nil {
		p.mu.Unlock()
		return nil
	}
	if p.secret == "" {
		p.mu.Unlock()
		return errors.New("connect your Playit account first")
	}
	p.enabled = true
	p.mu.Unlock()
	writeJSON(filepath.Join(p.dir, "enabled.json"), true)
	p.update("starting", "Preparing the official Playit agent")
	var release struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	b, e := embeddedAssets.ReadFile("assets/playit-release.json")
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &release); e != nil {
		return e
	}
	name := "playit-linux-amd64"
	if runtime.GOOS == "windows" {
		name = "playit-windows-x86_64.exe"
	} else if runtime.GOARCH == "arm64" {
		name = "playit-linux-aarch64"
	}
	target := filepath.Join(p.dir, name)
	found := false
	for _, asset := range release.Assets {
		if asset.Name == name {
			found = true
			if asset.Digest == "" {
				return errors.New("Playit release has no checksum")
			}
			valid := fileSHA256(target) == strings.TrimPrefix(asset.Digest, "sha256:")
			if !valid {
				if e = download(asset.URL, target, asset.Digest, nil); e != nil {
					return e
				}
			}
			break
		}
	}
	if !found {
		return errors.New("Playit is not available for this platform")
	}
	os.Chmod(target, 0755)
	socket := filepath.Join(p.dir, "agent.sock")
	if runtime.GOOS == "windows" {
		socket = `\\.\pipe\lanforge-playit-` + hashToken(p.dir)[:12]
	}
	cmd := exec.Command(target, "--secret-path", filepath.Join(p.dir, "secret"), "--socket-path", socket, "--log-path", filepath.Join(p.dir, "agent.log"))
	cmd.Dir = p.dir
	prepareProcess(cmd)
	f, e := os.OpenFile(filepath.Join(p.dir, "startup.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		f.Close()
		return e
	}
	f.Close()
	p.mu.Lock()
	p.cmd = cmd
	if proc, err := process.NewProcess(int32(cmd.Process.Pid)); err == nil {
		birth, _ := proc.CreateTime()
		writeJSON(filepath.Join(p.dir, "process.json"), processIdentity{cmd.Process.Pid, birth})
	}
	p.state.State = "running"
	p.state.Message = "Playit agent process running; public reachability requires a tunnel"
	p.mu.Unlock()
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.cmd = nil
		os.Remove(filepath.Join(p.dir, "process.json"))
		enabled := p.enabled
		p.mu.Unlock()
		if enabled {
			p.update("error", fmt.Sprintf("Playit agent exited: %v. It will retry automatically.", err))
		} else {
			p.update("stopped", "Playit agent stopped")
		}
	}()
	return nil
}
func (p *Playit) stop() error {
	p.mu.Lock()
	p.enabled = false
	cmd := p.cmd
	recovered := p.recovered
	birth := p.recoveredBirth
	p.mu.Unlock()
	writeJSON(filepath.Join(p.dir, "enabled.json"), false)
	if cmd != nil {
		return stopProcess(cmd, false)
	}
	if recovered != nil {
		current, err := recovered.CreateTime()
		if err == nil && current == birth {
			proc, err := os.FindProcess(int(recovered.Pid))
			if err != nil {
				return err
			}
			return stopProcess(&exec.Cmd{Process: proc}, false)
		}
	}
	p.update("stopped", "Playit agent stopped")
	return nil
}

// reset stops the agent and forgets the linked account so Connect Playit claims afresh.
func (p *Playit) reset() (any, error) {
	if e := p.stop(); e != nil {
		return nil, e
	}
	if e := os.Remove(filepath.Join(p.dir, "secret")); e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	p.mu.Lock()
	p.secret = ""
	p.claiming = false
	p.state = PlayitState{State: "stopped", Message: "Playit disconnected. Connect Playit to sign in again."}
	p.mu.Unlock()
	return p.snapshot(), nil
}
func (p *Playit) refresh() (any, error) {
	p.mu.Lock()
	key := p.secret
	p.mu.Unlock()
	if key == "" {
		return p.snapshot(), nil
	}
	data, e := playitAPI("/agents/rundata", key, map[string]any{})
	if e != nil {
		return nil, e
	}
	var raw map[string]any
	if e = json.Unmarshal(data, &raw); e != nil {
		return nil, e
	}
	arr := []map[string]any{}
	if list, ok := raw["tunnels"].([]any); ok {
		for _, x := range list {
			if t, ok := x.(map[string]any); ok {
				arr = append(arr, t)
			}
		}
	}
	p.mu.Lock()
	p.state.Tunnels = arr
	p.mu.Unlock()
	return raw, nil
}
func (p *Playit) tunnel(s Server) (any, error) {
	if s.Mode != "playit" {
		return nil, errors.New("this server was created in a different connection mode")
	}
	if e := p.start(); e != nil {
		return nil, e
	}
	if !p.op.TryLock() {
		return nil, errors.New("Playit is busy")
	}
	defer p.op.Unlock()
	raw, e := p.refresh()
	if e != nil {
		return nil, e
	}
	data, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("connect your Playit account first")
	}
	agentID := str(data["agent_id"])
	if agentID == "" {
		return nil, errors.New("Playit agent ID is missing")
	}
	p.mu.Lock()
	key := p.secret
	existing := clone(p.state.Tunnels)
	p.mu.Unlock()
	ports := s.Ports
	if s.Engine == "valheim" && len(ports) > 1 {
		ports = ports[:1]
	}
	for _, port := range ports {
		if !port.Public {
			continue
		}
		count := 1
		if s.Engine == "valheim" {
			count = 2
		}
		match := false
		for _, t := range existing {
			if str(t["local_ip"]) == "127.0.0.1" && str(t["local_port"]) == fmt.Sprint(port.Number) && str(t["proto"]) == port.Protocol && !asBool(t["disabled"]) {
				if pr, ok := t["port"].(map[string]any); ok {
					from, _ := toInt(pr["from"])
					to, _ := toInt(pr["to"])
					if to-from >= count {
						match = true
						break
					}
				}
			}
		}
		if match {
			continue
		}
		var typ any
		switch s.Engine {
		case "minecraft":
			typ = "minecraft-java"
		case "terraria":
			typ = "terraria"
		case "valheim":
			typ = "valheim"
		}
		body := map[string]any{"name": "LANForge " + s.Name + " " + port.Name, "tunnel_type": typ, "port_type": port.Protocol, "port_count": count, "enabled": true, "alloc": nil, "firewall_id": nil, "proxy_protocol": nil, "origin": map[string]any{"type": "agent", "data": map[string]any{"agent_id": agentID, "local_ip": "127.0.0.1", "local_port": port.Number}}}
		if _, e = playitAPI("/tunnels/create", key, body); e != nil {
			return nil, fmt.Errorf("%w. Check your Playit plan/account or create the tunnel at https://playit.gg/account/tunnels using local address 127.0.0.1:%d (%s). LANForge will detect it", e, port.Number, port.Protocol)
		}
	}
	_, e = p.refresh()
	if e != nil {
		return nil, e
	}
	return p.snapshot(), nil
}
func (p *Playit) loop(ctx context.Context) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		p.mu.Lock()
		enabled := p.enabled
		if p.recovered != nil {
			alive, err := p.recovered.IsRunning()
			birth, _ := p.recovered.CreateTime()
			if err != nil || !alive || birth != p.recoveredBirth {
				p.recovered = nil
				os.Remove(filepath.Join(p.dir, "process.json"))
				p.state.State = "stopped"
			}
		}
		running := p.cmd != nil || p.recovered != nil
		key := p.secret
		p.mu.Unlock()
		if enabled && !running && key != "" {
			if e := p.start(); e != nil {
				p.update("error", e.Error())
			}
		}
		if key != "" {
			if _, e := p.refresh(); e != nil {
				p.mu.Lock()
				p.state.Message = e.Error()
				p.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
