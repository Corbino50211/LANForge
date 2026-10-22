package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type session struct{ expires time.Time }
type pairing struct{ expires time.Time }
type peer struct {
	conn   *websocket.Conn
	send   chan Wire
	node   Node
	states []Runtime
	playit PlayitState
}
type Controller struct {
	data        string
	cfg         Config
	store       *Store
	catalog     *Catalog
	fingerprint string
	mu          sync.Mutex
	createMu    sync.Mutex
	dnsMu       sync.Mutex
	peers       map[string]*peer
	pending     map[string]chan Wire
	sessions    map[string]session
	pairs       map[string]pairing
	attempts    map[string][]time.Time
	refreshMu   sync.Mutex
}

func newController(data string, cfg Config) (*Controller, error) {
	s, e := openStore(data)
	if e != nil {
		return nil, e
	}
	cat, e := newCatalog(data)
	if e != nil {
		s.db.Close()
		return nil, e
	}
	fp, e := ensureCertificate(data)
	if e != nil {
		return nil, e
	}
	return &Controller{data: data, cfg: cfg, store: s, catalog: cat, fingerprint: fp, peers: map[string]*peer{}, pending: map[string]chan Wire{}, sessions: map[string]session{}, pairs: map[string]pairing{}, attempts: map[string][]time.Time{}}, nil
}
func hashToken(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, e error) {
	jsonReply(w, 400, map[string]string{"error": e.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	d := json.NewDecoder(r.Body)
	if e := d.Decode(v); e != nil {
		return errors.New("invalid request data")
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return errors.New("unexpected extra request data")
	}
	return nil
}
func (c *Controller) rate(key string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	a := c.attempts[key]
	j := 0
	for _, t := range a {
		if t.After(cut) {
			a[j] = t
			j++
		}
	}
	a = a[:j]
	if len(a) >= max {
		return false
	}
	c.attempts[key] = append(a, time.Now())
	if len(c.attempts) > 5000 {
		for k, v := range c.attempts {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(c.attempts, k)
			}
		}
	}
	return true
}
func (c *Controller) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(remote)
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			http.Error(w, "LANForge management is available only on the local network", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/agent/connect" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != "GET" {
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, e := url.Parse(origin)
				if e != nil || u.Host != r.Host || u.Scheme != "https" {
					jsonReply(w, 403, map[string]string{"error": "request origin rejected"})
					return
				}
			}
			if r.Header.Get("X-LanForge") != "1" {
				jsonReply(w, 403, map[string]string{"error": "missing request protection header"})
				return
			}
		}
		if r.URL.Path == "/api/login" || r.URL.Path == "/api/pair" || r.URL.Path == "/api/session" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, e := r.Cookie("lanforge")
		c.mu.Lock()
		s, ok := c.sessions[valueOfCookie(cookie)]
		if ok && time.Now().After(s.expires) {
			delete(c.sessions, valueOfCookie(cookie))
			ok = false
		}
		c.mu.Unlock()
		if e != nil || !ok {
			jsonReply(w, 401, map[string]string{"error": "Please sign in"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func valueOfCookie(c *http.Cookie) string {
	if c == nil {
		return ""
	}
	return c.Value
}
func (c *Controller) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !c.rate("login:"+ip, 20) {
			jsonReply(w, 429, map[string]string{"error": "Too many attempts. Try again in ten minutes."})
			return
		}
		var in struct {
			Password string `json:"password"`
		}
		if e := decode(w, r, &in); e != nil {
			apiError(w, e)
			return
		}
		var hash string
		if c.store.get("auth", "password", &hash) != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
			jsonReply(w, 401, map[string]string{"error": "Incorrect password"})
			return
		}
		t := token(32)
		c.mu.Lock()
		c.sessions[t] = session{time.Now().Add(24 * time.Hour)}
		c.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "lanforge", Value: t, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
		jsonReply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		ck, _ := r.Cookie("lanforge")
		c.mu.Lock()
		s, ok := c.sessions[valueOfCookie(ck)]
		c.mu.Unlock()
		jsonReply(w, 200, map[string]any{"authenticated": ok && time.Now().Before(s.expires), "name": c.cfg.Name, "version": Version})
	})
	m.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		ck, _ := r.Cookie("lanforge")
		c.mu.Lock()
		delete(c.sessions, valueOfCookie(ck))
		c.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "lanforge", Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		jsonReply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /api/state", c.state)
	m.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, c.catalog.list()) })
	m.HandleFunc("POST /api/templates/refresh", func(w http.ResponseWriter, r *http.Request) {
		if !c.refreshMu.TryLock() {
			apiError(w, errors.New("a refresh is already running"))
			return
		}
		go func() {
			defer c.refreshMu.Unlock()
			e := c.catalog.refresh()
			msg := "Template catalog refreshed"
			if e != nil {
				msg = e.Error()
			}
			c.store.put("notice", "catalog", map[string]any{"message": msg, "time": time.Now(), "error": e != nil})
		}()
		jsonReply(w, 202, map[string]string{"message": "Refreshing catalog in the background"})
	})
	m.HandleFunc("POST /api/pairing", func(w http.ResponseWriter, r *http.Request) {
		code := strings.ToUpper(token(4))
		c.mu.Lock()
		for k, v := range c.pairs {
			if time.Now().After(v.expires) {
				delete(c.pairs, k)
			}
		}
		c.pairs[code] = pairing{time.Now().Add(10 * time.Minute)}
		c.mu.Unlock()
		jsonReply(w, 200, map[string]any{"code": code, "fingerprint": c.fingerprint, "addresses": controllerURLs(c.cfg.Port), "expiresIn": 600})
	})
	m.HandleFunc("POST /api/pair", c.pair)
	m.HandleFunc("POST /api/nodes/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "local" {
			apiError(w, errors.New("the controller's host cannot be removed"))
			return
		}
		c.mu.Lock()
		if p := c.peers[id]; p != nil {
			p.conn.Close()
		}
		c.mu.Unlock()
		if e := c.store.del("nodeToken", id); e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /api/agent/connect", c.connect)
	m.HandleFunc("POST /api/servers", c.createServer)
	m.HandleFunc("POST /api/servers/{id}/{action}", c.serverAction)
	m.HandleFunc("POST /api/nodes/{id}/playit/{action}", func(w http.ResponseWriter, r *http.Request) {
		act := r.PathValue("action")
		if act != "connect" && act != "refresh" && act != "start" && act != "stop" {
			apiError(w, errors.New("unknown action"))
			return
		}
		res, e := c.call(r.Context(), r.PathValue("id"), Command{Action: "playit." + act})
		if e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, 200, res)
	})
	m.HandleFunc("POST /api/nodes/{id}/steam/{action}", func(w http.ResponseWriter, r *http.Request) {
		act := r.PathValue("action")
		if act != "login" && act != "logout" {
			apiError(w, errors.New("unknown action"))
			return
		}
		args := map[string]string{}
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&args)
		res, e := c.call(r.Context(), r.PathValue("id"), Command{Action: "steam." + act, Args: args})
		if e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, 200, res)
	})
	m.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var zone string
		c.store.get("settings", "cfZone", &zone)
		key, _ := c.store.unsecret("cfToken")
		jsonReply(w, 200, map[string]any{"cloudflareZone": zone, "cloudflareConnected": key != "", "fingerprint": c.fingerprint, "port": c.cfg.Port, "addresses": controllerURLs(c.cfg.Port), "dataDirectory": c.data})
	})
	m.HandleFunc("POST /api/settings/cloudflare", c.cloudflareSettings)
	m.HandleFunc("POST /api/settings/password", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Current  string `json:"current"`
			Password string `json:"password"`
		}
		if e := decode(w, r, &in); e != nil {
			apiError(w, e)
			return
		}
		var old string
		c.store.get("auth", "password", &old)
		if bcrypt.CompareHashAndPassword([]byte(old), []byte(in.Current)) != nil {
			apiError(w, errors.New("current password is incorrect"))
			return
		}
		if len(in.Password) < 10 || len(in.Password) > 72 {
			apiError(w, errors.New("use a password between 10 and 72 characters"))
			return
		}
		h, e := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if e == nil {
			e = c.store.put("auth", "password", string(h))
		}
		if e != nil {
			apiError(w, e)
			return
		}
		c.mu.Lock()
		c.sessions = map[string]session{}
		c.mu.Unlock()
		jsonReply(w, 200, map[string]bool{"ok": true})
	})
	ui, _ := fs.Sub(webAssets, "ui/dist")
	m.Handle("/", http.FileServer(http.FS(ui)))
	return c.auth(m)
}
func (c *Controller) pair(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !c.rate("pair:"+ip, 10) {
		jsonReply(w, 429, map[string]string{"error": "Too many pairing attempts"})
		return
	}
	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
		OS   string `json:"os"`
		Arch string `json:"arch"`
	}
	if e := decode(w, r, &in); e != nil {
		apiError(w, e)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	c.mu.Lock()
	p, ok := c.pairs[code]
	if ok && time.Now().Before(p.expires) {
		delete(c.pairs, code)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		jsonReply(w, 403, map[string]string{"error": "Pairing code is invalid or expired. Generate a new one on the controller."})
		return
	}
	if len(in.Name) > 80 || in.Name == "" {
		in.Name = "Host"
	}
	id := token(12)
	key := token(32)
	n := Node{ID: id, Name: in.Name, OS: in.OS, Arch: in.Arch, Address: ip}
	if e := c.store.put("nodeToken", id, hashToken(key)); e != nil {
		apiError(w, e)
		return
	}
	if e := c.store.put("nodes", id, n); e != nil {
		apiError(w, e)
		return
	}
	jsonReply(w, 200, map[string]string{"id": id, "token": key})
}
func (c *Controller) connect(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var stored string
	if c.store.get("nodeToken", id, &stored) != nil || subtle.ConstantTimeCompare([]byte(hashToken(auth)), []byte(stored)) != 1 {
		http.Error(w, "Host is not paired", 401)
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	ws, e := up.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	p := &peer{conn: ws, send: make(chan Wire, 32)}
	var n Node
	c.store.get("nodes", id, &n)
	p.node = n
	c.mu.Lock()
	if old := c.peers[id]; old != nil {
		old.conn.Close()
	}
	c.peers[id] = p
	c.mu.Unlock()
	done := make(chan struct{})
	defer func() {
		close(done)
		ws.Close()
		c.mu.Lock()
		if c.peers[id] == p {
			delete(c.peers, id)
		}
		c.mu.Unlock()
	}()
	ws.SetReadLimit(16 << 20)
	ws.SetReadDeadline(time.Now().Add(35 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(35 * time.Second)) })
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case msg := <-p.send:
				ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if ws.WriteJSON(msg) != nil {
					ws.Close()
					return
				}
			case <-tick.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					ws.Close()
					return
				}
			}
		}
	}()
	for {
		var msg Wire
		if e := ws.ReadJSON(&msg); e != nil {
			return
		}
		ws.SetReadDeadline(time.Now().Add(35 * time.Second))
		switch msg.Type {
		case "heartbeat":
			if msg.Node != nil {
				msg.Node.ID = id
				msg.Node.Online = true
				msg.Node.LastSeen = time.Now()
				host, _, _ := net.SplitHostPort(r.RemoteAddr)
				if id == "local" {
					host = localAddress(c.cfg.Port)
				}
				msg.Node.Address = host
				c.mu.Lock()
				p.node = *msg.Node
				p.states = msg.Servers
				if msg.Playit != nil {
					p.playit = *msg.Playit
				}
				c.mu.Unlock()
				c.store.put("nodes", id, *msg.Node)
			}
		case "result":
			c.mu.Lock()
			ch := c.pending[msg.ID]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
		}
	}
}
func (c *Controller) call(ctx context.Context, node string, cmd Command) (any, error) {
	cmd.ID = token(16)
	ch := make(chan Wire, 1)
	c.mu.Lock()
	p := c.peers[node]
	if p == nil {
		c.mu.Unlock()
		return nil, errors.New("this computer is offline; turn it on and start LANForge")
	}
	c.pending[cmd.ID] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, cmd.ID); c.mu.Unlock() }()
	select {
	case p.send <- Wire{Type: "command", ID: cmd.ID, Command: &cmd}:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("host is busy; try again shortly")
	}
	select {
	case result := <-ch:
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		return result.Data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(120 * time.Second):
		return nil, errors.New("host did not acknowledge in time; check its status before retrying")
	}
}
func (c *Controller) state(w http.ResponseWriter, r *http.Request) {
	var nodes []Node
	var servers []Server
	var notices []map[string]any
	c.store.list("nodes", &nodes)
	c.store.list("servers", &servers)
	c.store.list("notice", &notices)
	states := map[string]Runtime{}
	playit := map[string]PlayitState{}
	c.mu.Lock()
	for i := range nodes {
		if p := c.peers[nodes[i].ID]; p != nil {
			nodes[i] = p.node
			for _, s := range p.states {
				states[s.ID] = s
			}
			playit[nodes[i].ID] = p.playit
		} else {
			nodes[i].Online = false
		}
	}
	c.mu.Unlock()
	for i := range servers {
		servers[i].Plan = nil
	}
	jsonReply(w, 200, map[string]any{"nodes": nodes, "servers": servers, "runtime": states, "playit": playit, "notices": notices, "version": Version})
}
func validateServer(s *Server, t Template) error {
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 80 {
		return errors.New("server name must be 1–80 characters")
	}
	if s.Mode != "lan" && s.Mode != "playit" && s.Mode != "direct" {
		return errors.New("choose a connection mode")
	}
	for k, v := range s.Settings {
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("invalid setting: %s", k)
		}
	}
	if v := s.Settings["world"]; v != "" {
		if !regexpWorld.MatchString(v) {
			return errors.New("world name may contain letters, numbers, spaces, underscores and dashes only")
		}
	}
	for _, p := range s.Ports {
		if p.Number < 1024 || p.Number > 65535 {
			return errors.New("game ports must be between 1024 and 65535")
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" && p.Protocol != "both" {
			return errors.New("unsupported port protocol")
		}
	}
	for i, p := range s.Ports {
		for _, q := range s.Ports[i+1:] {
			if p.Number == q.Number && protocolOverlap(p.Protocol, q.Protocol) {
				return errors.New("two server ports overlap; choose distinct ports")
			}
		}
	}
	if t.Engine == "minecraft" {
		if s.Settings["eula"] != "true" {
			return errors.New("accept the Minecraft EULA before installation")
		}
		m, e := strconv.Atoi(s.Settings["memory"])
		if e != nil || m < 512 || m > 131072 {
			return errors.New("Minecraft memory must be 512–131072 MB")
		}
	}
	if t.Engine == "valheim" {
		pw := s.Settings["password"]
		if len(pw) < 5 || strings.Contains(strings.ToLower(s.Name), strings.ToLower(pw)) {
			return errors.New("Valheim password needs at least five characters and must not be part of the server name")
		}
	}
	return nil
}
func protocolOverlap(a, b string) bool { return a == b || a == "both" || b == "both" }
func (c *Controller) createServer(w http.ResponseWriter, r *http.Request) {
	c.createMu.Lock()
	defer c.createMu.Unlock()
	var s Server
	if e := decode(w, r, &s); e != nil {
		apiError(w, e)
		return
	}
	t, e := c.catalog.get(s.TemplateID)
	if e != nil {
		apiError(w, e)
		return
	}
	var n Node
	if c.store.get("nodes", s.NodeID, &n) != nil {
		apiError(w, errors.New("choose a paired computer"))
		return
	}
	supported := false
	for _, p := range t.OS {
		if p == n.OS {
			supported = true
		}
	}
	if !supported {
		apiError(w, errors.New("this game does not support that computer's operating system"))
		return
	}
	if n.Arch != "amd64" {
		apiError(w, errors.New("automatic game installation currently requires an x86-64 host"))
		return
	}
	provided := s.Settings
	s.Settings = map[string]string{}
	for _, f := range t.Fields {
		s.Settings[f.ID] = f.Default
	}
	for k, v := range provided {
		s.Settings[k] = v
	}
	if len(s.Ports) == 0 {
		s.Ports = clone(t.Ports)
	}
	if len(s.Ports) != len(t.Ports) {
		apiError(w, errors.New("all template ports must be reserved"))
		return
	}
	for i := range s.Ports {
		s.Ports[i].ID = t.Ports[i].ID
		s.Ports[i].Name = t.Ports[i].Name
		s.Ports[i].Protocol = t.Ports[i].Protocol
		s.Ports[i].Public = t.Ports[i].Public
	}
	if t.Engine == "valheim" && s.Ports[1].Number != s.Ports[0].Number+1 {
		apiError(w, errors.New("Valheim query port must be game port + 1"))
		return
	}
	if e = validateServer(&s, t); e != nil {
		apiError(w, e)
		return
	}
	var existing []Server
	c.store.list("servers", &existing)
	for _, p := range s.Ports {
		for _, old := range existing {
			if old.NodeID == s.NodeID {
				for _, q := range old.Ports {
					if p.Number == q.Number && protocolOverlap(p.Protocol, q.Protocol) {
						apiError(w, fmt.Errorf("port %d is already reserved by %s", p.Number, old.Name))
						return
					}
				}
			}
		}
	}
	s.ID = token(12)
	s.Game = t.Name
	s.Engine = t.Engine
	s.Revision = t.Revision
	s.Created = time.Now()
	s.DNS = nil
	if s.Settings["$ExternalIP"] == "" {
		s.Settings["$ExternalIP"] = n.Address
	}
	s.Plan, e = compilePlan(t, s, n.OS)
	if e != nil {
		apiError(w, e)
		return
	}
	if e = c.store.put("servers", s.ID, s); e != nil {
		apiError(w, e)
		return
	}
	_, e = c.call(r.Context(), s.NodeID, Command{Action: "install", Server: &s})
	if e != nil {
		c.store.put("notice", s.ID, map[string]any{"message": s.Name + ": " + e.Error(), "time": time.Now(), "error": true})
		apiError(w, fmt.Errorf("server entry saved; installation could not be acknowledged: %w. Use Install / repair on its page", e))
		return
	}
	s.Plan = nil
	jsonReply(w, 201, s)
}
func (c *Controller) serverAction(w http.ResponseWriter, r *http.Request) {
	var s Server
	if c.store.get("servers", r.PathValue("id"), &s) != nil {
		jsonReply(w, 404, map[string]string{"error": "Server not found"})
		return
	}
	act := r.PathValue("action")
	var args map[string]string
	if e := decode(w, r, &args); e != nil {
		apiError(w, e)
		return
	}
	if act == "dns" {
		if e := c.configureDNS(&s, args); e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, 200, s.DNS)
		return
	}
	if act == "settings" {
		c.createMu.Lock()
		defer c.createMu.Unlock()
		if err := c.store.get("servers", s.ID, &s); err != nil {
			apiError(w, err)
			return
		}
		copy := clone(s)
		for k, v := range args {
			if k == "autoStart" {
				copy.AutoStart = v == "true"
			} else if k == "restartOnCrash" {
				copy.RestartOnCrash = v == "true"
			} else {
				copy.Settings[k] = v
			}
		}
		t, e := c.catalog.get(s.TemplateID)
		if e != nil {
			apiError(w, e)
			return
		}
		if e = validateServer(&copy, t); e != nil {
			apiError(w, e)
			return
		}
		if copy.Engine == "amp" {
			apiError(w, errors.New("edit configuration files for experimental AMP servers; automatic setting updates are not supported yet"))
			return
		}
		res, e := c.call(r.Context(), s.NodeID, Command{Action: "settings", Server: &copy})
		if e != nil {
			apiError(w, e)
			return
		}
		if e = c.store.put("servers", s.ID, copy); e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, 200, res)
		return
	}
	allowed := map[string]bool{"install": true, "start": true, "stop": true, "restart": true, "console": true, "files": true, "read": true, "write": true, "upload": true, "backups": true, "backup": true, "restore": true, "tunnel": true}
	if !allowed[act] {
		apiError(w, errors.New("unknown server action"))
		return
	}
	cmd := Command{Action: act, ServerID: s.ID, Args: args}
	if act == "install" {
		cmd.Server = &s
	}
	res, e := c.call(r.Context(), s.NodeID, cmd)
	if e != nil {
		apiError(w, e)
		return
	}
	jsonReply(w, 200, res)
}

// VirtualBox/VMware/Hyper-V/WSL/Docker adapters have private addresses other computers cannot reach.
var virtualAdapterRE = regexp.MustCompile(`(?i)virtualbox|vmware|vmnet|vbox|vethernet|hyper-v|wsl|docker|br-|veth|virbr|host-only`)

func controllerURLs(port int) []string {
	out := []string{fmt.Sprintf("https://localhost:%d", port)}
	// The address of the default-route interface comes first; UDP dial sends no packets.
	if c, e := net.Dial("udp4", "1.1.1.1:53"); e == nil {
		if ip := c.LocalAddr().(*net.UDPAddr).IP; ip.IsPrivate() {
			out = append(out, fmt.Sprintf("https://%s:%d", ip, port))
		}
		c.Close()
	}
	ifs, _ := net.Interfaces()
	for _, n := range ifs {
		if n.Flags&net.FlagUp == 0 || n.Flags&net.FlagLoopback != 0 || virtualAdapterRE.MatchString(n.Name) {
			continue
		}
		a, _ := n.Addrs()
		for _, v := range a {
			ip, _, e := net.ParseCIDR(v.String())
			u := fmt.Sprintf("https://%s:%d", ip, port)
			// 192.168.56.0/24 is VirtualBox's host-only network, which Windows names "Ethernet N".
			if e == nil && ip.To4() != nil && ip.IsPrivate() && !strings.HasPrefix(ip.String(), "192.168.56.") && !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	}
	return out
}
func localAddress(port int) string {
	for _, raw := range controllerURLs(port) {
		u, e := url.Parse(raw)
		if e == nil && u.Hostname() != "localhost" {
			return u.Hostname()
		}
	}
	return "127.0.0.1"
}
func (c *Controller) serve(ctx context.Context) error {
	srv := &http.Server{Addr: fmt.Sprintf(":%d", c.cfg.Port), Handler: c.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	go c.dnsLoop(ctx)
	go updateLoop(ctx, func() bool { return true })
	go func() {
		tick := time.NewTicker(24 * time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if c.refreshMu.TryLock() {
					if e := c.catalog.refresh(); e != nil {
						log.Printf("Catalog refresh: %s", e)
					}
					c.refreshMu.Unlock()
				}
			}
		}
	}()
	log.Printf("LANForge %s: %s", Version, controllerURLs(c.cfg.Port))
	return srv.ListenAndServeTLS(filepath.Join(c.data, "tls.crt"), filepath.Join(c.data, "tls.key"))
}

var _ = runtime.GOOS
var _ = os.ErrNotExist
