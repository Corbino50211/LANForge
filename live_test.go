package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Optional real-download test. It does not require a Playit or DNS account.
func TestLiveTerraria(t *testing.T) {
	if os.Getenv("LANFORGE_LIVE_TEST") != "1" {
		t.Skip("set LANFORGE_LIVE_TEST=1 to download and launch official Terraria")
	}
	dir := os.Getenv("LANFORGE_LIVE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	h, e := newHost(dir, Config{})
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	s := Server{ID: "live-terraria", Name: "LANForge test", Engine: "terraria", Ports: []Port{{ID: "game", Number: port, Protocol: "tcp", Public: true}}, Settings: map[string]string{"world": "LANForgeTest", "size": "1", "maxPlayers": "4", "password": ""}, Plan: &Plan{Engine: "terraria"}}
	m := newManaged(filepath.Join(dir, "servers", s.ID), s)
	os.MkdirAll(m.root, 0700)
	go m.followLogs()
	if e = installGame(h, m); e != nil {
		t.Fatal(e)
	}
	writeJSON(filepath.Join(m.root, "installed.json"), map[string]bool{"ok": true})
	if e = m.start(); e != nil {
		t.Fatal(e)
	}
	defer m.stop()
	end := time.Now().Add(4 * time.Minute)
	ready := false
	for time.Now().Before(end) {
		if m.snapshot().State == "running" {
			ready = true
			break
		}
		if m.snapshot().State == "error" {
			t.Fatalf("Terraria exited: %+v", m.snapshot())
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatalf("no ready signal: %+v", m.snapshot())
	}
	conn, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	conn.Close()
	if e = m.console("say LANForge integration test"); e != nil {
		t.Fatal(e)
	}
	if e = m.stop(); e != nil {
		t.Fatal(e)
	}
	t.Log("Official Terraria installed, generated a world, announced readiness, accepted a TCP connection and stopped through its console")
}

func TestLiveJava(t *testing.T) {
	if os.Getenv("LANFORGE_LIVE_TEST") != "1" {
		t.Skip("opt-in Java runtime download")
	}
	dir := os.Getenv("LANFORGE_LIVE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	m := newManaged(filepath.Join(dir, "java-test"), Server{})
	os.MkdirAll(m.root, 0700)
	java, e := installJava(m, 21)
	if e != nil {
		t.Fatal(e)
	}
	output, e := exec.Command(filepath.Join(m.root, java), "-version").CombinedOutput()
	if e != nil {
		t.Fatal(e, string(output))
	}
	t.Log(string(output))
}
