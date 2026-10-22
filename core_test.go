package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPathConfinement(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../outside", "/etc/passwd", "C:\\Windows\\x", "a/../../bad"} {
		if _, e := inside(root, p); e == nil {
			t.Errorf("accepted unsafe path %q", p)
		}
	}
	if p, e := inside(root, "world/region.dat"); e != nil || !strings.HasPrefix(p, root) {
		t.Fatal(p, e)
	}
	if runtime.GOOS != "windows" {
		os.Symlink(t.TempDir(), filepath.Join(root, "link"))
		if _, e := inside(root, "link/file"); e == nil {
			t.Fatal("followed symlink")
		}
	}
}
func TestArchiveTraversal(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			a := filepath.Join(dir, "bad."+kind)
			f, _ := os.Create(a)
			if kind == "zip" {
				z := zip.NewWriter(f)
				w, _ := z.Create("../escape.txt")
				w.Write([]byte("bad"))
				z.Close()
			} else {
				g := gzip.NewWriter(f)
				tr := tar.NewWriter(g)
				tr.WriteHeader(&tar.Header{Name: "../escape.txt", Size: 3, Mode: 0600})
				tr.Write([]byte("bad"))
				tr.Close()
				g.Close()
			}
			f.Close()
			dest := filepath.Join(dir, "dest")
			os.Mkdir(dest, 0700)
			if e := extract(a, dest); e == nil {
				t.Fatal("accepted traversal archive")
			}
			if _, e := os.Stat(filepath.Join(dir, "escape.txt")); e == nil {
				t.Fatal("wrote outside destination")
			}
		})
	}
}
func TestStoreAndSecretPersistence(t *testing.T) {
	dir := t.TempDir()
	s, e := openStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.secret("test", "private-value"); e != nil {
		t.Fatal(e)
	}
	s.put("items", "one", map[string]string{"name": "persisted"})
	s.db.Close()
	s, e = openStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.db.Close()
	got, e := s.unsecret("test")
	if e != nil || got != "private-value" {
		t.Fatal(got, e)
	}
	var sealed string
	s.get("secrets", "test", &sealed)
	if strings.Contains(sealed, "private-value") {
		t.Fatal("secret stored unencrypted")
	}
	var items []map[string]string
	if e = s.list("items", &items); e != nil || len(items) != 1 {
		t.Fatal(items, e)
	}
}
func TestCatalogAndCompile(t *testing.T) {
	c, e := newCatalog(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	all := c.list()
	if len(all) < 240 {
		t.Fatalf("incomplete catalog: %d", len(all))
	}
	for _, id := range []string{"minecraft-java", "terraria", "valheim"} {
		g, e := c.get(id)
		if e != nil || g.Status != "supported" {
			t.Fatal(id, e)
		}
	}
	b := parseKVP([]byte("key=a=b=c\n#comment\nApp.Name=Test\n"))
	if b["key"] != "a=b=c" {
		t.Fatal(b)
	}
	ports := parsePorts([]map[string]any{{"Port": float64(2456), "Protocol": "UDP", "Ref": "game", "ChildPorts": []any{map[string]any{"Port": float64(1000), "Offset": float64(1), "Ref": "query", "Protocol": "UDP"}}}})
	if len(ports) != 2 || ports[1].Number != 2457 {
		t.Fatal(ports)
	}
	supported := 0
	for _, g := range all {
		if g.Status == "experimental" {
			s := Server{Settings: map[string]string{}, Ports: g.Ports}
			for _, f := range g.Fields {
				s.Settings[f.ID] = f.Default
			}
			if _, e := compilePlan(g, s, "linux"); e == nil {
				supported++
			}
		}
	}
	t.Logf("Catalog: %d templates, %d declarative Linux plans compile", len(all), supported)
	if supported < 5 {
		t.Fatal("generic template compatibility regressed")
	}
}
func TestPreserveProperties(t *testing.T) {
	p := filepath.Join(t.TempDir(), "server.properties")
	os.WriteFile(p, []byte("# my comment\nunknown=keep\nserver-port=1234\n"), 0600)
	if e := mergeProperties(p, map[string]string{"server-port": "25565", "motd": "My World"}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{"# my comment", "unknown=keep", "server-port=25565", "motd=My World"} {
		if !strings.Contains(string(b), want) {
			t.Fatal(string(b))
		}
	}
}
func testController(t *testing.T) (*Controller, *httptest.Server, *http.Client) {
	t.Helper()
	dir := t.TempDir()
	cfg, e := initController(dir, "Test PC", "test-password-123", 18443)
	if e != nil {
		t.Fatal(e)
	}
	c, e := newController(dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s := httptest.NewTLSServer(c.routes())
	jar, _ := cookiejar.New(nil)
	client := s.Client()
	client.Jar = jar
	t.Cleanup(func() { s.Close(); c.store.db.Close() })
	return c, s, client
}
func reqJSON(t *testing.T, client *http.Client, method, raw string, body any) (int, map[string]any) {
	t.Helper()
	var b io.Reader
	if body != nil {
		v, _ := json.Marshal(body)
		b = bytes.NewReader(v)
	}
	req, _ := http.NewRequest(method, raw, b)
	req.Header.Set("X-LanForge", "1")
	req.Header.Set("Content-Type", "application/json")
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}
func TestAuthPairingAndPin(t *testing.T) {
	_, s, client := testController(t)
	code, _ := reqJSON(t, client, "GET", s.URL+"/api/state", nil)
	if code != 401 {
		t.Fatal(code)
	}
	code, _ = reqJSON(t, client, "POST", s.URL+"/api/login", map[string]string{"password": "wrong"})
	if code != 401 {
		t.Fatal(code)
	}
	code, _ = reqJSON(t, client, "POST", s.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if code != 200 {
		t.Fatal(code)
	}
	code, pair := reqJSON(t, client, "POST", s.URL+"/api/pairing", map[string]any{})
	if code != 200 {
		t.Fatal(pair)
	}
	sum := sha256.Sum256(s.Certificate().Raw)
	pin := hex.EncodeToString(sum[:])
	cfg, e := pairHost(context.Background(), s.URL, pin, pair["code"].(string), "Laptop")
	if e != nil || cfg.Token == "" {
		t.Fatal(e)
	}
	if _, e = pairHost(context.Background(), s.URL, pin, pair["code"].(string), "Replay"); e == nil {
		t.Fatal("reused one-time pairing code")
	}
	if _, e = pairHost(context.Background(), s.URL, strings.Repeat("00", 32), "anything", "Bad pin"); e == nil {
		t.Fatal("accepted wrong TLS pin")
	}
	req, _ := http.NewRequest("POST", s.URL+"/api/logout", bytes.NewBufferString("{}"))
	req.Header.Set("X-LanForge", "1")
	req.Header.Set("Origin", "https://attacker.invalid")
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("accepted cross-origin mutation", res.StatusCode)
	}
}
func TestHostReconnectAndCommands(t *testing.T) {
	c, s, client := testController(t)
	reqJSON(t, client, "POST", s.URL+"/api/login", map[string]string{"password": "test-password-123"})
	_, pair := reqJSON(t, client, "POST", s.URL+"/api/pairing", map[string]any{})
	sum := sha256.Sum256(s.Certificate().Raw)
	cfg, e := pairHost(context.Background(), s.URL, hex.EncodeToString(sum[:]), pair["code"].(string), "Remote laptop")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := t.TempDir()
	go runHost(ctx, hostDir, cfg)
	waitFor(t, 15*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		p := c.peers[cfg.AgentID]
		return p != nil && p.node.Online
	})
	_, e = c.call(ctx, cfg.AgentID, Command{Action: "playit.refresh"})
	if e != nil {
		t.Fatal(e)
	}
	c.mu.Lock()
	old := c.peers[cfg.AgentID]
	old.conn.Close()
	c.mu.Unlock()
	waitFor(t, 15*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		p := c.peers[cfg.AgentID]
		return p != nil && p != old && p.node.Online
	})
	_, e = c.call(ctx, cfg.AgentID, Command{Action: "files", ServerID: "missing"})
	if e == nil || !strings.Contains(e.Error(), "not installed") {
		t.Fatal(e)
	}
	// Deleting a server removes its folder on the host and its dashboard record.
	gone := filepath.Join(hostDir, "servers", "deleteme")
	os.MkdirAll(filepath.Join(gone, "game"), 0700)
	os.WriteFile(filepath.Join(gone, "game", "world.dat"), []byte("x"), 0600)
	c.store.put("servers", "deleteme", Server{ID: "deleteme", NodeID: cfg.AgentID, Name: "Doomed"})
	if code, body := reqJSON(t, client, "POST", s.URL+"/api/servers/deleteme/delete", map[string]any{}); code != 200 {
		t.Fatal("delete failed", code, body)
	}
	var sv Server
	if _, e := os.Stat(gone); !os.IsNotExist(e) || c.store.get("servers", "deleteme", &sv) == nil {
		t.Fatal("server not deleted", e)
	}
	if code, _ := reqJSON(t, client, "POST", s.URL+"/api/nodes/"+cfg.AgentID+"/revoke", map[string]any{}); code != 200 {
		t.Fatal("revoke failed", code)
	}
	// The host keeps retrying with its revoked token; it must stay gone.
	time.Sleep(3 * time.Second)
	var n Node
	c.mu.Lock()
	p := c.peers[cfg.AgentID]
	c.mu.Unlock()
	if p != nil || c.store.get("nodes", cfg.AgentID, &n) == nil {
		t.Fatal("removed computer came back")
	}
}
func waitFor(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if pred() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
func TestManagedHelper(t *testing.T) {
	if os.Getenv("LANFORGE_TEST_HELPER") != "yes" {
		return
	}
	fmt.Println("helper ready")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if sc.Text() == "stop" {
			os.Exit(0)
		}
		fmt.Println("echo: " + sc.Text())
	}
	os.Exit(0)
}
func helperManaged(t *testing.T) *Managed {
	t.Helper()
	// Not t.TempDir: Windows may still lock the copied helper.exe briefly after exit, failing cleanup.
	root, e := os.MkdirTemp("", "lanforge-helper-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	os.MkdirAll(filepath.Join(root, "game"), 0700)
	exe, _ := os.Executable()
	r, e := os.Open(exe)
	if e != nil {
		t.Fatal(e)
	}
	name := "helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	w, e := os.OpenFile(filepath.Join(root, "game", name), os.O_CREATE|os.O_WRONLY, 0755)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(w, r)
	w.Close()
	r.Close()
	s := Server{ID: "testserver", Name: "Test", Engine: "amp", Settings: map[string]string{}, Plan: &Plan{Engine: "amp", Executable: "game/" + name, WorkDir: "game", Args: []string{"-test.run=TestManagedHelper"}, Stop: "stop", Env: map[string]string{"LANFORGE_TEST_HELPER": "yes"}}}
	writeJSON(filepath.Join(root, "installed.json"), map[string]bool{"ok": true})
	return newManaged(root, s)
}
func TestProcessLifecycleAndBackup(t *testing.T) {
	m := helperManaged(t)
	if e := m.start(); e != nil {
		t.Fatal(e)
	}
	pid := m.snapshot().PID
	if pid == 0 {
		t.Fatal("process not running")
	}
	if e := m.start(); e != nil || m.snapshot().PID != pid {
		t.Fatal("duplicate process started")
	}
	if e := m.console("hello"); e != nil {
		t.Fatal(e)
	}
	if _, e := m.backup(); e == nil {
		t.Fatal("backed up a running server")
	}
	if e := m.stop(); e != nil {
		t.Fatal(e)
	}
	if m.snapshot().PID != 0 {
		t.Fatal("process still running")
	}
	p := filepath.Join(m.root, "game", "world.txt")
	os.WriteFile(p, []byte("original world"), 0600)
	res, e := m.backup()
	if e != nil {
		t.Fatal(e)
	}
	name := res.(map[string]string)["name"]
	waitFor(t, 15*time.Second, func() bool { return m.snapshot().State == "stopped" })
	os.WriteFile(p, []byte("changed world"), 0600)
	if e = m.restore(name); e != nil {
		t.Fatal(e)
	}
	waitFor(t, 15*time.Second, func() bool { return m.snapshot().State == "stopped" })
	b, e := os.ReadFile(p)
	if e != nil || string(b) != "original world" {
		t.Fatal(string(b), e)
	}
	old, _ := filepath.Glob(filepath.Join(m.root, "before-restore-*", "world.txt"))
	if len(old) != 1 {
		t.Fatal("old world not preserved")
	}
	b, _ = os.ReadFile(old[0])
	if string(b) != "changed world" {
		t.Fatal(string(b))
	}
}
func TestPortConflict(t *testing.T) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	m := newManaged(t.TempDir(), Server{Ports: []Port{{Number: l.Addr().(*net.TCPAddr).Port, Protocol: "tcp"}}})
	if e = m.checkPorts(); e == nil {
		t.Fatal("failed to detect occupied port")
	}
}
func TestSettingsKeepInstalledPlan(t *testing.T) {
	h, e := newHost(t.TempDir(), Config{})
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(h.dir, "servers", "mc")
	m := newManaged(root, Server{ID: "mc", Engine: "minecraft", Settings: map[string]string{"eula": "true", "memory": "1024", "maxPlayers": "10", "world": "world"}, Ports: []Port{{Number: 25565, Protocol: "tcp"}}, Plan: &Plan{Engine: "minecraft", Executable: "runtime/bin/java", WorkDir: "game", Stop: "stop"}})
	h.servers["mc"] = m
	incoming := m.specCopy()
	incoming.Plan = &Plan{Engine: "minecraft"}
	incoming.Settings["memory"] = "2048"
	if _, e = h.handle(Command{Action: "settings", Server: &incoming}); e != nil {
		t.Fatal(e)
	}
	if m.specCopy().Plan.Executable != "runtime/bin/java" {
		t.Fatal("lost installed runtime path")
	}
}

func TestAMPConfigMapping(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "server.ini")
	os.WriteFile(p, []byte(";keep comment\n[Server]\nname=old\nunknown=stay\n"), 0600)
	if e := mergeINI(p, map[string]string{"Server\x1fname": "new", "Network\x1fport": "12345"}, ""); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	for _, v := range []string{";keep comment", "name=new", "unknown=stay", "[Network]", "port=12345"} {
		if !strings.Contains(string(b), v) {
			t.Fatal(string(b))
		}
	}
	j := filepath.Join(dir, "server.json")
	os.WriteFile(j, []byte(`{"server":{"players":4,"enabled":false},"unknown":"keep"}`), 0600)
	if e := mergeJSON(j, map[string]string{"server.players": "12", "server.enabled": "true"}); e != nil {
		t.Fatal(e)
	}
	var obj map[string]any
	readJSON(j, &obj)
	server := obj["server"].(map[string]any)
	if server["players"] != float64(12) || server["enabled"] != true || obj["unknown"] != "keep" {
		t.Fatal(obj)
	}
}
func TestTemplateArguments(t *testing.T) {
	args, e := splitArgs(`-name "My \"quoted\" server" -path C:\\Games\\world`)
	if e != nil || len(args) != 4 || args[1] != `My "quoted" server` {
		t.Fatal(args, e)
	}
}

func TestArchiveInternalLinks(t *testing.T) {
	for _, target := range []string{"../LICENSE", "../../../escape", "/etc/passwd"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "runtime.tar.gz")
			f, _ := os.Create(archive)
			g := gzip.NewWriter(f)
			w := tar.NewWriter(g)
			body := []byte("runtime license")
			w.WriteHeader(&tar.Header{Name: "runtime/LICENSE", Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			w.Write(body)
			w.WriteHeader(&tar.Header{Name: "runtime/legal/LICENSE", Linkname: target, Typeflag: tar.TypeSymlink})
			w.Close()
			g.Close()
			f.Close()
			dest := filepath.Join(dir, "out")
			err := extract(archive, dest)
			if target != "../LICENSE" {
				if err == nil {
					t.Fatal("unsafe link accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "runtime/legal/LICENSE"))
			if err != nil || string(got) != string(body) {
				t.Fatal("internal link was not materialized", err)
			}
			st, _ := os.Lstat(filepath.Join(dest, "runtime/legal/LICENSE"))
			if !st.Mode().IsRegular() {
				t.Fatal("expected regular file")
			}
		})
	}
}
func TestStageHelpers(t *testing.T) {
	var doc any
	json.Unmarshal([]byte(`{"assets":[{"u":"a"},{"u":"https://x/b.zip"}],"recommended_download":"https://x/c"}`), &doc)
	for p, want := range map[string]string{"$.assets.[1].u": "https://x/b.zip", "$.assets[1].u": "https://x/b.zip", "$.recommended_download": "https://x/c"} {
		if got, e := jsonPath(doc, p); e != nil || got != want {
			t.Fatal(p, got, e)
		}
	}
	p := filepath.Join(t.TempDir(), "server.cfg")
	os.WriteFile(p, []byte("// keep\nset hostname \"old\"\nset hostname2 \"x\"\n"), 0600)
	if e := mergeFormatted(p, `set {0} "{1}"`, map[string]string{"hostname": "new", "maxplayers": "8"}); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(p); string(b) != "// keep\nset hostname \"new\"\nset hostname2 \"x\"\nset maxplayers \"8\"\n" {
		t.Fatalf("%q", b)
	}
	if v, ok := templateFunc("Windows:a.exe, Linux:a", "linux"); !ok || v != "a" {
		t.Fatal(v)
	}
	if v, _ := templateFunc("randomdigits(6)", "linux"); len(v) != 6 || strings.Trim(v, "0123456789") != "" {
		t.Fatal(v)
	}
}

func TestMinecraftPropertiesAndWhitelist(t *testing.T) {
	m := newManaged(t.TempDir(), Server{Engine: "minecraft", Name: "Test", Plan: &Plan{}, Ports: []Port{{Number: 25570}}, Settings: map[string]string{"eula": "true", "memory": "1024", "maxPlayers": "5", "world": "w", "properties": "difficulty=hard\nonline-mode=false\nserver-port=1", "whitelist": "Steve\nAlex"}})
	if e := configureGame(m); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(m.root, "game", "server.properties"))
	for _, want := range []string{"difficulty=hard", "server-port=25570", "white-list=true", "max-players=5"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("server.properties missing %s:\n%s", want, b)
		}
	}
	var list []map[string]string
	if e := readJSON(filepath.Join(m.root, "game", "whitelist.json"), &list); e != nil || len(list) != 2 || list[0]["name"] != "Steve" {
		t.Fatalf("whitelist: %v %v", list, e)
	}
	// Offline UUID for "Steve", as the vanilla server computes it.
	if list[0]["uuid"] != "5627dd98-e6be-3c21-b8a8-e92344183641" {
		t.Fatalf("offline uuid %s", list[0]["uuid"])
	}
	if _, e := minecraftWhitelist([]string{"bad name!"}, false); e == nil {
		t.Fatal("accepted invalid name")
	}
}

func TestMergeJSONWithComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("{\n  // server name\n  \"name\": \"a // not a comment\", /* block */\n  \"port\": 1,\n  \"url\": \"http://x\",\n}\n"), 0600)
	if e := mergeJSON(p, map[string]string{"port": "5557"}); e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	if e := readJSON(p, &got); e != nil || got["port"] != float64(5557) || got["name"] != "a // not a comment" || got["url"] != "http://x" {
		t.Fatalf("%v %v", got, e)
	}
}
