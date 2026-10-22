package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func installGame(h *Host, m *Managed) error {
	os.MkdirAll(filepath.Join(m.root, "game"), 0700)
	s := m.specCopy()
	switch s.Engine {
	case "minecraft":
		if e := installMinecraft(m); e != nil {
			return e
		}
	case "terraria":
		if e := installTerraria(m); e != nil {
			return e
		}
	case "valheim":
		if e := steamInstall(h, m, "896660", filepath.Join(m.root, "game"), "", "", false); e != nil {
			return e
		}
	case "app":
		if e := installApp(m); e != nil {
			return e
		}
	case "amp":
		if s.Plan == nil {
			return errors.New("missing AMP installation plan")
		}
		if e := runSteps(h, m, s.Plan.Steps); e != nil {
			return e
		}
		exe, e := inside(m.root, s.Plan.Executable)
		if e != nil {
			return e
		}
		if e = os.Chmod(exe, 0755); e != nil {
			return e
		}
		return applyAMPConfigs(m)
	default:
		return errors.New("unknown game installer")
	}
	return configureGame(m)
}
func installJava(m *Managed, major int) (string, error) {
	marker := filepath.Join(m.root, fmt.Sprintf("java-%d.json", major))
	var cached string
	if readJSON(marker, &cached) == nil {
		if p, e := inside(m.root, cached); e == nil {
			if _, e = os.Stat(p); e == nil {
				return cached, nil
			}
		}
	}
	m.log(fmt.Sprintf("Installing Java %d locally for this server", major))
	var assets []struct {
		Binary struct {
			Package struct {
				Link     string `json:"link"`
				Checksum string `json:"checksum"`
				Name     string `json:"name"`
			} `json:"package"`
		} `json:"binary"`
	}
	api := fmt.Sprintf("https://api.adoptium.net/v3/assets/latest/%d/hotspot?architecture=x64&image_type=jre&os=%s", major, runtime.GOOS)
	if e := fetchJSON(api, &assets); e != nil {
		return "", fmt.Errorf("Java download metadata: %w", e)
	}
	if len(assets) == 0 {
		return "", errors.New("no Java runtime available for this platform")
	}
	pkg := assets[0].Binary.Package
	if filepath.Base(pkg.Name) != pkg.Name {
		return "", errors.New("invalid runtime archive name")
	}
	archive := filepath.Join(m.root, "downloads", pkg.Name)
	if e := download(pkg.Link, archive, pkg.Checksum, m.log); e != nil {
		return "", e
	}
	dest := filepath.Join(m.root, fmt.Sprintf("runtime-%d", major))
	os.MkdirAll(dest, 0700)
	if e := extract(archive, dest); e != nil {
		return "", e
	}
	name := "java"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	found := ""
	filepath.WalkDir(dest, func(path string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && d.Name() == name && filepath.Base(filepath.Dir(path)) == "bin" {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", errors.New("Java archive did not contain bin/java")
	}
	os.Chmod(found, 0755)
	rel, _ := filepath.Rel(m.root, found)
	rel = filepath.ToSlash(rel)
	writeJSON(marker, rel)
	return rel, nil
}
func installTerraria(m *Managed) error {
	m.log("Downloading official Terraria dedicated server")
	var names []string
	if e := fetchJSON("https://terraria.org/api/get/dedicated-servers-names", &names); e != nil {
		return e
	}
	if len(names) == 0 || filepath.Base(names[0]) != names[0] || !strings.HasSuffix(names[0], ".zip") {
		return errors.New("unexpected Terraria download response")
	}
	archive := filepath.Join(m.root, "downloads", names[0])
	if e := download("https://terraria.org/api/download/pc-dedicated-server/"+url.PathEscape(names[0]), archive, "", m.log); e != nil {
		return e
	}
	stage, e := os.MkdirTemp(m.root, "terraria-stage-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if e = extract(archive, stage); e != nil {
		return e
	}
	platform := "Linux"
	exe := "TerrariaServer.bin.x86_64"
	if runtime.GOOS == "windows" {
		platform = "Windows"
		exe = "TerrariaServer.exe"
	}
	src := ""
	filepath.WalkDir(stage, func(p string, d os.DirEntry, e error) error {
		if e == nil && d.IsDir() && d.Name() == platform {
			src = p
			return filepath.SkipDir
		}
		return nil
	})
	if src == "" {
		return errors.New("Terraria archive missing platform folder")
	}
	if e = copyTree(src, filepath.Join(m.root, "game")); e != nil {
		return e
	}
	for _, n := range []string{"TerrariaServer", "TerrariaServer.bin.x86_64"} {
		if runtime.GOOS == "linux" {
			os.Chmod(filepath.Join(m.root, "game", n), 0755)
		}
	}
	m.mu.Lock()
	m.spec.Plan = &Plan{Engine: "terraria", Executable: "game/" + exe, WorkDir: "game", Stop: "exit", Args: []string{"-config", "serverconfig.txt"}}
	m.mu.Unlock()
	return nil
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not copied")
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		target, e := inside(dst, rel)
		if e != nil {
			return e
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		a, e := os.Open(p)
		if e != nil {
			return e
		}
		defer a.Close()
		st, e := d.Info()
		if e != nil {
			return e
		}
		b, e := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
		if e != nil {
			return e
		}
		_, e = io.Copy(b, a)
		ce := b.Close()
		if e != nil {
			return e
		}
		return ce
	})
}
func ensureSteamCMD(h *Host, log func(string)) (string, string, error) {
	dir := filepath.Join(h.dir, "tools", "steamcmd")
	exe := filepath.Join(dir, "steamcmd.sh")
	if runtime.GOOS == "windows" {
		exe = filepath.Join(dir, "steamcmd.exe")
	}
	if _, e := os.Stat(exe); e != nil {
		log("Installing Valve SteamCMD")
		u := "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"
		name := "steamcmd.tar.gz"
		if runtime.GOOS == "windows" {
			u = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"
			name = "steamcmd.zip"
		}
		archive := filepath.Join(h.dir, "downloads", name)
		if e = download(u, archive, "", log); e != nil {
			return "", "", e
		}
		os.MkdirAll(dir, 0700)
		if e = extract(archive, dir); e != nil {
			return "", "", e
		}
		os.Chmod(exe, 0755)
	}
	return dir, exe, nil
}

// steamUser is the account signed in with steam.login; SteamCMD caches its login token.
func steamUser(h *Host) string {
	b, _ := os.ReadFile(filepath.Join(h.dir, "steam-user.txt"))
	return strings.TrimSpace(string(b))
}

// steamLogin signs SteamCMD in once so later installs can use the cached login without the password.
func steamLogin(h *Host, user, pass, code string) (any, error) {
	if user == "" || strings.ContainsAny(user, " \t\r\n") {
		return nil, errors.New("enter your Steam username")
	}
	// SteamCMD can't run twice at once; installs hold installMu for minutes.
	if !h.installMu.TryLock() {
		return nil, errors.New("wait for the current game installation to finish, then sign in")
	}
	defer h.installMu.Unlock()
	dir, exe, e := ensureSteamCMD(h, func(string) {})
	if e != nil {
		return nil, e
	}
	args := []string{"+login", user, pass, "+quit"}
	if code != "" {
		args = append([]string{"+set_steam_guard_code", code}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	prepareProcess(cmd)
	out, _ := cmd.CombinedOutput()
	text := string(out)
	if !strings.Contains(text, "Logged in OK") && !strings.Contains(text, "Waiting for user info...OK") {
		switch {
		case ctx.Err() != nil:
			return nil, errors.New("Steam sign-in timed out. Approve the sign-in in the Steam mobile app, or enter the Steam Guard code from your app or email, then try again")
		case strings.Contains(text, "Two-factor") || strings.Contains(text, "Steam Guard") || strings.Contains(text, "TwoFactor") || strings.Contains(text, "AccountLogonDenied"):
			return nil, errors.New("Steam Guard code required or wrong. Enter the current code from your Steam app or email")
		case strings.Contains(text, "Invalid Password") || strings.Contains(text, "InvalidPassword"):
			return nil, errors.New("Steam rejected the username or password")
		}
		lines := strings.Split(strings.TrimSpace(text), "\n")
		return nil, errors.New("Steam sign-in failed: " + strings.TrimSpace(lines[len(lines)-1]))
	}
	if e = atomicWrite(filepath.Join(h.dir, "steam-user.txt"), []byte(user), 0600); e != nil {
		return nil, e
	}
	return map[string]string{"user": user}, nil
}
func steamInstall(h *Host, m *Managed, appid, dest, branch, platform string, login bool) error {
	dir, exe, e := ensureSteamCMD(h, m.log)
	if e != nil {
		return e
	}
	account := "anonymous"
	if login {
		if account = steamUser(h); account == "" {
			return errors.New("this game needs a Steam account that owns it. Open Computers and choose Steam sign-in on this computer, then install again")
		}
	}
	os.MkdirAll(dest, 0700)
	args := []string{"+force_install_dir", dest, "+login", account, "+app_update", appid}
	if platform != "" {
		args = append([]string{"+@sSteamCmdForcePlatformType", platform}, args...)
	}
	if branch != "" && branch != "public" {
		args = append(args, "-beta", branch)
	}
	args = append(args, "validate", "+quit")
	m.log("Installing Steam application " + appid + ". This can take several minutes.")
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	prepareProcess(cmd)
	f, e := os.OpenFile(filepath.Join(m.root, "console.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Run(); e != nil {
		if login {
			return fmt.Errorf("SteamCMD failed: %w. Make sure the signed-in Steam account owns this game; if the sign-in expired, sign in to Steam again on the Computers page", e)
		}
		return fmt.Errorf("SteamCMD failed: %w. On Linux, install the distribution's 32-bit C/C++ runtime (Ubuntu: sudo apt install lib32gcc-s1 lib32stdc++6). Check the console for the exact error", e)
	}
	return nil
}

// minecraftWhitelist builds whitelist.json entries. Online servers need the Mojang UUID; offline servers use the name-derived one.
func minecraftWhitelist(names []string, online bool) ([]map[string]string, error) {
	list := []map[string]string{}
	for _, name := range names {
		if !minecraftNameRE.MatchString(name) {
			return nil, fmt.Errorf("%q is not a valid Minecraft player name", name)
		}
		var id string
		if online {
			var p struct{ ID, Name string }
			if e := fetchJSON("https://api.mojang.com/users/profiles/minecraft/"+name, &p); e != nil || len(p.ID) != 32 {
				return nil, fmt.Errorf("could not find Minecraft player %s; check the spelling", name)
			}
			id, name = p.ID, p.Name
		} else {
			b := md5.Sum([]byte("OfflinePlayer:" + name))
			b[6], b[8] = b[6]&0x0f|0x30, b[8]&0x3f|0x80
			id = hex.EncodeToString(b[:])
		}
		list = append(list, map[string]string{"uuid": id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:], "name": name})
	}
	return list, nil
}

var minecraftNameRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

func configureGame(m *Managed) error {
	s := m.specCopy()
	if len(s.Ports) == 0 {
		return errors.New("no game port configured")
	}
	port := strconv.Itoa(s.Ports[0].Number)
	game := filepath.Join(m.root, "game")
	os.MkdirAll(game, 0700)
	switch s.Engine {
	case "minecraft":
		if s.Plan == nil {
			return nil
		}
		s.Plan.Args = append([]string{"-Xms512M", "-Xmx" + s.Settings["memory"] + "M"}, minecraftArgs(s)...)
		if s.Settings["eula"] != "true" {
			return errors.New("Minecraft EULA has not been accepted")
		}
		if e := atomicWrite(filepath.Join(game, "eula.txt"), []byte("eula=true\n"), 0600); e != nil {
			return e
		}
		props := map[string]string{"server-ip": "", "motd": s.Name, "online-mode": "true", "enable-rcon": "false"}
		for k, v := range parseKVP([]byte(s.Settings["properties"])) {
			props[k] = v
		}
		names := strings.FieldsFunc(s.Settings["whitelist"], func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' })
		if len(names) > 0 {
			props["white-list"], props["enforce-whitelist"] = "true", "true"
		}
		// These belong to other fields and the reserved port.
		props["server-port"], props["max-players"], props["level-name"] = port, s.Settings["maxPlayers"], s.Settings["world"]
		if e := mergeProperties(filepath.Join(game, "server.properties"), props); e != nil {
			return e
		}
		if _, ok := s.Settings["whitelist"]; ok {
			list, e := minecraftWhitelist(names, props["online-mode"] != "false")
			if e != nil {
				return e
			}
			if e = writeJSON(filepath.Join(game, "whitelist.json"), list); e != nil {
				return e
			}
		}
	case "app":
		p, e := appPlan(s, m.root)
		if e != nil {
			return e
		}
		s.Plan = p
	case "terraria":
		os.MkdirAll(filepath.Join(game, "worlds"), 0700)
		if e := mergeProperties(filepath.Join(game, "serverconfig.txt"), map[string]string{"world": "worlds/" + s.Settings["world"] + ".wld", "worldname": s.Settings["world"], "autocreate": s.Settings["size"], "port": port, "maxplayers": s.Settings["maxPlayers"], "password": s.Settings["password"], "upnp": "0", "secure": "1"}); e != nil {
			return e
		}
	case "valheim":
		exe := "valheim_server.x86_64"
		if runtime.GOOS == "windows" {
			exe = "valheim_server.exe"
		}
		os.Chmod(filepath.Join(game, exe), 0755)
		s.Plan = &Plan{Engine: "valheim", Executable: "game/" + exe, WorkDir: "game", Args: []string{"-nographics", "-batchmode", "-name", s.Name, "-port", port, "-world", s.Settings["world"], "-password", s.Settings["password"], "-public", s.Settings["public"], "-savedir", "Saves"}, Env: map[string]string{"SteamAppId": "892970", "LD_LIBRARY_PATH": "$ROOT/game/linux64"}}
	}
	m.mu.Lock()
	m.spec = s
	m.mu.Unlock()
	return nil
}
func mergeProperties(path string, updates map[string]string) error {
	lines := []string{}
	seen := map[string]bool{}
	if f, e := os.Open(path); e == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 4096), 2<<20)
		for sc.Scan() {
			line := sc.Text()
			key, _, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if v, exists := updates[key]; ok && exists && !strings.HasPrefix(key, "#") {
				lines = append(lines, key+"="+v)
				seen[key] = true
			} else {
				lines = append(lines, line)
			}
		}
		e = sc.Err()
		f.Close()
		if e != nil {
			return e
		}
	}
	for k, v := range updates {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	return atomicWrite(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
}
