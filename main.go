package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/gofrs/flock"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed ui/dist/*
var webAssets embed.FS

//go:embed assets/setup.html
var setupHTML string

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
func toInt(v any) (int, error)       { return strconv.Atoi(str(v)) }
func fileSHA256(path string) string {
	f, e := os.Open(path)
	if e != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
func defaultData() string {
	p, e := os.UserConfigDir()
	if e != nil {
		p = "."
	}
	return filepath.Join(p, "LANForge")
}
func openBrowser(u string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		c = exec.Command("open", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	c.Start()
}
func initController(dir, name, password string, port int) (Config, error) {
	cfg := Config{Role: "controller", Name: name, Port: port}
	if port < 1024 || port > 65535 {
		return cfg, errors.New("choose a management port between 1024 and 65535")
	}
	if len(password) < 10 || len(password) > 72 {
		return cfg, errors.New("choose a password between 10 and 72 characters")
	}
	if _, e := os.Stat(filepath.Join(dir, "config.json")); e == nil {
		return cfg, errors.New("LANForge is already configured in this folder")
	}
	s, e := openStore(dir)
	if e != nil {
		return cfg, e
	}
	defer s.db.Close()
	hash, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if e != nil {
		return cfg, e
	}
	if e = s.put("auth", "password", string(hash)); e != nil {
		return cfg, e
	}
	if _, e = ensureCertificate(dir); e != nil {
		return cfg, e
	}
	return cfg, writeJSON(filepath.Join(dir, "config.json"), cfg)
}
func (c *Controller) localHost() error {
	dir := filepath.Join(c.data, "local-host")
	var cfg Config
	if e := readJSON(filepath.Join(dir, "config.json"), &cfg); e != nil {
		cfg = Config{Role: "host", Name: c.cfg.Name, Controller: fmt.Sprintf("https://127.0.0.1:%d", c.cfg.Port), Fingerprint: c.fingerprint, AgentID: "local", Token: token(32)}
		if e = c.store.put("nodeToken", "local", hashToken(cfg.Token)); e != nil {
			return e
		}
		if e = c.store.put("nodes", "local", Node{ID: "local", Name: cfg.Name, OS: runtime.GOOS, Arch: runtime.GOARCH, Address: "127.0.0.1"}); e != nil {
			return e
		}
		if e = writeJSON(filepath.Join(dir, "config.json"), cfg); e != nil {
			return e
		}
	}
	lock := flock.New(filepath.Join(dir, "host.lock"))
	ok, e := lock.TryLock()
	if e != nil {
		return e
	}
	if !ok {
		return nil
	}
	lock.Unlock()
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(exe, "host", "--data", dir, "--no-browser")
	detachProcess(cmd)
	f, e := os.OpenFile(filepath.Join(dir, "host.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	cmd.Stdout = f
	cmd.Stderr = f
	e = cmd.Start()
	f.Close()
	if e != nil {
		return e
	}
	go cmd.Wait()
	return nil
}
func bootstrap(dir string, noBrowser bool) (Config, error) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return Config{}, e
	}
	secret := token(24)
	result := make(chan Config, 1)
	m := http.NewServeMux()
	m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		io.WriteString(w, setupHTML)
	})
	m.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Setup-Token") != secret {
			http.Error(w, "Setup token required", 403)
			return
		}
		var in struct {
			Role        string `json:"role"`
			Name        string `json:"name"`
			Password    string `json:"password"`
			Port        int    `json:"port"`
			Controller  string `json:"controller"`
			Fingerprint string `json:"fingerprint"`
			Code        string `json:"code"`
		}
		if e := decode(w, r, &in); e != nil {
			apiError(w, e)
			return
		}
		if in.Name == "" {
			in.Name, _ = os.Hostname()
		}
		var cfg Config
		var err error
		if in.Role == "controller" {
			cfg, err = initController(dir, in.Name, in.Password, in.Port)
		} else if in.Role == "host" {
			cfg, err = pairHost(r.Context(), in.Controller, in.Fingerprint, in.Code, in.Name)
			if err == nil {
				err = writeJSON(filepath.Join(dir, "config.json"), cfg)
			}
		} else {
			err = errors.New("choose controller or host")
		}
		if err != nil {
			apiError(w, err)
			return
		}
		next := cfg.Controller
		if cfg.Role == "controller" {
			next = fmt.Sprintf("https://localhost:%d", cfg.Port)
		}
		jsonReply(w, 200, map[string]string{"url": next, "role": cfg.Role})
		select {
		case result <- cfg:
		default:
		}
	})
	srv := &http.Server{Handler: m, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	u := "http://" + ln.Addr().String() + "/#" + secret
	fmt.Println("First-time setup: " + u)
	if !noBrowser {
		openBrowser(u)
	}
	cfg := <-result
	time.AfterFunc(4*time.Second, func() { srv.Close() })
	return cfg, nil
}
func main() {
	if e := runMain(); e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Print(e)
		os.Exit(1)
	}
}
func runMain() error {
	args := os.Args[1:]
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("lanforge", flag.ContinueOnError)
	dir := flags.String("data", defaultData(), "local data folder")
	noBrowser := flags.Bool("no-browser", false, "do not open a browser")
	name := flags.String("name", "My PC", "computer name")
	port := flags.Int("port", 8443, "controller port for init")
	if e := flags.Parse(args); e != nil {
		return e
	}
	full, e := filepath.Abs(*dir)
	if e != nil {
		return e
	}
	*dir = full
	os.MkdirAll(*dir, 0700)
	if command == "version" {
		fmt.Println(Version)
		return nil
	}
	if command == "init" {
		password := os.Getenv("LANFORGE_PASSWORD")
		if password == "" {
			return errors.New("set LANFORGE_PASSWORD for noninteractive init; normal launch opens the setup wizard")
		}
		_, e := initController(*dir, *name, password, *port)
		return e
	}
	if command != "run" && command != "host" {
		return errors.New("commands: run, host, init, version")
	}
	var cfg Config
	if e = readJSON(filepath.Join(*dir, "config.json"), &cfg); e != nil {
		if command == "host" {
			return e
		}
		cfg, e = bootstrap(*dir, *noBrowser)
		if e != nil {
			return e
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, appCancel = context.WithCancel(ctx)
	// Registered before the locks so it runs after they are released.
	defer func() {
		if restartRequested.Load() {
			if e := relaunch(filepath.Join(*dir, "lanforge.log")); e != nil {
				log.Printf("Restart after update: %s", e)
			}
		}
	}()
	if cfg.Role == "host" || command == "host" {
		return runHost(ctx, *dir, cfg)
	}
	lock := flock.New(filepath.Join(*dir, "controller.lock"))
	ok, e := lock.TryLock()
	if e != nil {
		return e
	}
	if !ok {
		if !*noBrowser {
			openBrowser(fmt.Sprintf("https://localhost:%d", cfg.Port))
		}
		return nil
	}
	defer lock.Unlock()
	c, e := newController(*dir, cfg)
	if e != nil {
		return e
	}
	defer c.store.db.Close()
	if e = c.localHost(); e != nil {
		return e
	}
	// Bring the local host back if it exits; localHost does nothing while one holds the lock.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if e := c.localHost(); e != nil {
				log.Printf("Local host: %s", e)
			}
		}
	}()
	if !*noBrowser {
		time.AfterFunc(time.Second, func() { openBrowser(fmt.Sprintf("https://localhost:%d", cfg.Port)) })
	}
	return c.serve(ctx)
}
