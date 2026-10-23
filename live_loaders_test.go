package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Real downloads: LANFORGE_LIVE_TEST=1 go test -run LiveMinecraft -timeout 60m
func TestLiveMinecraftLoaders(t *testing.T) {
	if os.Getenv("LANFORGE_LIVE_TEST") != "1" {
		t.Skip("set LANFORGE_LIVE_TEST=1 to download and start each Minecraft loader")
	}
	mc := liveEnv("LANFORGE_LIVE_MC", "1.21.4")
	for _, l := range strings.Split(liveEnv("LANFORGE_LIVE_LOADERS", "paper,purpur,fabric,quilt,forge,neoforge"), ",") {
		t.Run(l, func(t *testing.T) { liveMinecraft(t, map[string]string{"loader": l, "version": mc}, "") })
	}
}
func TestLiveMinecraftMods(t *testing.T) {
	if os.Getenv("LANFORGE_LIVE_TEST") != "1" {
		t.Skip("set LANFORGE_LIVE_TEST=1")
	}
	t.Run("fabric+ledger", func(t *testing.T) {
		m := liveMinecraft(t, map[string]string{"loader": "fabric", "version": "1.21.4"}, "ledger")
		r, _ := modsList(m)
		if n := len(r.(map[string]any)["mods"].([]modRecord)); n < 3 {
			t.Fatalf("expected ledger plus its 2 required dependencies, got %d", n)
		}
	})
	t.Run("wizard mods", func(t *testing.T) {
		m := liveMinecraft(t, map[string]string{"loader": "fabric", "version": "1.21.4", "mods": "lithium,not-a-real-mod-xyz"}, "")
		r, _ := modsList(m)
		if n := len(r.(map[string]any)["mods"].([]modRecord)); n != 1 {
			t.Fatalf("expected lithium only, got %d", n)
		}
	})
	t.Run("modpack", func(t *testing.T) {
		liveMinecraft(t, map[string]string{"modpack": liveEnv("LANFORGE_LIVE_MODPACK", "vanilla-perfected")}, "")
	})
}

// liveMinecraft installs a server, optionally adds a Modrinth project, starts it and waits for "Done (".
func liveMinecraft(t *testing.T, settings map[string]string, mod string) *Managed {
	dir := t.TempDir()
	h, e := newHost(dir, Config{})
	if e != nil {
		t.Fatal(e)
	}
	l, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for k, v := range map[string]string{"eula": "true", "memory": "3072", "maxPlayers": "4", "world": "world"} {
		settings[k] = v
	}
	s := Server{ID: "live-mc", Name: "LANForge test", Engine: "minecraft", Ports: []Port{{ID: "game", Number: port, Protocol: "tcp", Public: true}}, Settings: settings, Plan: &Plan{Engine: "minecraft"}}
	m := newManaged(filepath.Join(dir, "servers", s.ID), s)
	os.MkdirAll(m.root, 0700)
	consoleTail := func() string {
		b, _ := os.ReadFile(filepath.Join(m.root, "console.log"))
		if len(b) > 3000 {
			b = b[len(b)-3000:]
		}
		return string(b)
	}
	if e = installGame(h, m); e != nil {
		t.Fatalf("install: %v\n%s", e, consoleTail())
	}
	writeJSON(filepath.Join(m.root, "installed.json"), map[string]bool{"ok": true})
	if mod != "" {
		if _, e = modsInstall(m, mod); e != nil {
			t.Fatalf("mod install: %v", e)
		}
	}
	if e = m.start(); e != nil {
		t.Fatalf("start: %v\n%s", e, consoleTail())
	}
	defer func() {
		m.stop()
		time.Sleep(10 * time.Second)
	}()
	end := time.Now().Add(8 * time.Minute)
	for time.Now().Before(end) {
		c := consoleTail()
		if strings.Contains(c, "Done (") {
			t.Logf("%s %s started: %s", m.specCopy().Settings["installedLoader"], m.specCopy().Settings["installedVersion"], strings.Join(minecraftArgs(m.specCopy()), " "))
			return m
		}
		if m.snapshot().PID == 0 && m.snapshot().State != "starting" && m.snapshot().State != "running" {
			t.Fatalf("server exited: %+v\n%s", m.snapshot(), c)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("server did not finish starting\n%s", consoleTail())
	return nil
}

// Real downloads: LANFORGE_LIVE_TEST=1 go test -run LiveApps -timeout 30m
func TestLiveApps(t *testing.T) {
	if os.Getenv("LANFORGE_LIVE_TEST") != "1" {
		t.Skip("set LANFORGE_LIVE_TEST=1")
	}
	apps := map[string]map[string]string{
		"node":   {"package.json": `{"name":"t","scripts":{"start":"node index.js"}}`, "index.js": `require('http').createServer((q,r)=>r.end('lanforge-ok')).listen(process.env.PORT)`},
		"python": {"main.py": "import http.server,os\nclass H(http.server.BaseHTTPRequestHandler):\n def do_GET(s):\n  s.send_response(200);s.end_headers();s.wfile.write(b'lanforge-ok')\nhttp.server.HTTPServer(('',int(os.environ['PORT'])),H).serve_forever()\n"},
		"go":     {"go.mod": "module t\n\ngo 1.21\n", "main.go": "package main\nimport (\"net/http\";\"os\")\nfunc main(){http.ListenAndServe(\":\"+os.Getenv(\"PORT\"),http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.Write([]byte(\"lanforge-ok\"))}))}\n"},
	}
	for rt, files := range apps {
		t.Run(rt, func(t *testing.T) {
			dir := t.TempDir()
			h, _ := newHost(dir, Config{})
			l, _ := net.Listen("tcp4", "127.0.0.1:0")
			port := l.Addr().(*net.TCPAddr).Port
			l.Close()
			s := Server{ID: "live-app", Name: "app", Engine: "app", Ports: []Port{{ID: "game", Number: port, Protocol: "tcp"}}, Settings: map[string]string{"runtime": rt, "source": "empty"}}
			m := newManaged(filepath.Join(dir, "servers", s.ID), s)
			for n, c := range files {
				os.MkdirAll(filepath.Join(m.root, "game"), 0700)
				os.WriteFile(filepath.Join(m.root, "game", n), []byte(c), 0600)
			}
			tail := func() string { b, _ := os.ReadFile(filepath.Join(m.root, "console.log")); return string(b) }
			if e := installGame(h, m); e != nil {
				t.Fatalf("install: %v\n%s", e, tail())
			}
			writeJSON(filepath.Join(m.root, "installed.json"), map[string]bool{"ok": true})
			if e := m.start(); e != nil {
				t.Fatalf("start: %v\n%s", e, tail())
			}
			defer func() { m.stop(); time.Sleep(2 * time.Second) }()
			for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
				if r, e := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)); e == nil {
					b, _ := io.ReadAll(r.Body)
					r.Body.Close()
					if string(b) == "lanforge-ok" {
						return
					}
				}
			}
			t.Fatalf("app never answered\n%s", tail())
		})
	}
}

func TestLiveSteamIcons(t *testing.T) {
	if os.Getenv("LANFORGE_LIVE_TEST") != "1" {
		t.Skip("set LANFORGE_LIVE_TEST=1")
	}
	icons := steamIcons(t.TempDir(), []string{"4000", "892970", "1"})
	if !strings.HasPrefix(icons["4000"], "https://cdn.cloudflare.steamstatic.com/") || icons["1"] != "" {
		t.Fatal(icons)
	}
}
