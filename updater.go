package main

import (
	"bufio"
	"context"
	"errors"
	"github.com/gofrs/flock"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// UpdateRepo is the GitHub "owner/name" whose latest release LANForge installs.
// Set it here or at build time with -ldflags "-X main.UpdateRepo=owner/name". Empty disables auto-update.
var UpdateRepo = "Corbino50211/LANForge"

// Each release must carry lanforge-<os>-<arch>[.exe] assets and a SHA256SUMS.txt covering them.
func updateAsset() string {
	n := "lanforge-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}

var appCancel context.CancelFunc
var restartRequested atomic.Bool

func newerVersion(latest, current string) bool {
	a, b := strings.Split(strings.TrimPrefix(latest, "v"), "."), strings.Split(strings.TrimPrefix(current, "v"), ".")
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := 0, 0
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			y, _ = strconv.Atoi(b[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// installUpdate replaces the executable on disk with a newer verified release.
// It returns true when a restart will run the new version.
func installUpdate() (bool, error) {
	if UpdateRepo == "" {
		return false, nil
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if e := fetchJSON("https://api.github.com/repos/"+UpdateRepo+"/releases/latest", &rel); e != nil {
		return false, e
	}
	if !newerVersion(rel.Tag, Version) {
		return false, nil
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	if urls[updateAsset()] == "" || urls["SHA256SUMS.txt"] == "" {
		return false, errors.New("release " + rel.Tag + " is missing " + updateAsset() + " or SHA256SUMS.txt")
	}
	exe, e := os.Executable()
	if e != nil {
		return false, e
	}
	if exe, e = filepath.EvalSymlinks(exe); e != nil {
		return false, e
	}
	sums := exe + ".sums-" + token(4)
	defer os.Remove(sums)
	if e = download(urls["SHA256SUMS.txt"], sums, "", nil); e != nil {
		return false, e
	}
	expected := ""
	if f, e := os.Open(sums); e == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if fs := strings.Fields(sc.Text()); len(fs) == 2 && strings.TrimPrefix(fs[1], "*") == updateAsset() {
				expected = strings.ToLower(fs[0])
			}
		}
		f.Close()
	}
	if len(expected) != 64 {
		return false, errors.New("SHA256SUMS.txt has no checksum for " + updateAsset())
	}
	// The controller and its local host share one executable; the lock makes the second wait and find the swap done.
	lock := flock.New(exe + ".update-lock")
	if e = lock.Lock(); e != nil {
		return false, e
	}
	defer lock.Unlock()
	if fileSHA256(exe) == expected {
		return true, nil
	}
	tmp := exe + ".new-" + token(4)
	defer os.Remove(tmp)
	log.Printf("Downloading LANForge %s", rel.Tag)
	if e = download(urls[updateAsset()], tmp, expected, nil); e != nil {
		return false, e
	}
	os.Chmod(tmp, 0755)
	// Windows allows renaming, but not overwriting, a running executable.
	old := exe + ".old-" + token(4)
	if e = os.Rename(exe, old); e != nil {
		return false, e
	}
	if e = os.Rename(tmp, exe); e != nil {
		os.Rename(old, exe)
		return false, e
	}
	log.Printf("Installed LANForge %s; restarting when idle", rel.Tag)
	return true, nil
}

// updateLoop checks for releases and restarts the process once idle reports nothing would be interrupted.
func updateLoop(ctx context.Context, idle func() bool) {
	if UpdateRepo == "" {
		return
	}
	if exe, e := os.Executable(); e == nil {
		old, _ := filepath.Glob(exe + ".old-*")
		for _, f := range old {
			os.Remove(f)
		}
	}
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 6 * time.Hour
		ready, e := installUpdate()
		if e != nil {
			log.Printf("Update check: %s", e)
			continue
		}
		if !ready {
			continue
		}
		for !idle() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
		restartRequested.Store(true)
		if appCancel != nil {
			appCancel()
		}
		return
	}
}

// relaunch starts the updated executable with the same arguments after this process has shut down.
func relaunch(logPath string) error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	args := os.Args[1:]
	hasNoBrowser := false
	for _, a := range args {
		hasNoBrowser = hasNoBrowser || a == "--no-browser" || a == "-no-browser"
	}
	if !hasNoBrowser {
		args = append(args, "--no-browser")
	}
	cmd := exec.Command(exe, args...)
	detachProcess(cmd)
	f, e := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	return cmd.Start()
}
