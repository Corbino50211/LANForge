package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runSteps executes compiled AMP stages in order. h is nil for pre-start stages.
func runSteps(h *Host, m *Managed, steps []Step) error {
	for _, st := range steps {
		m.log("Stage: " + st.Name)
		if e := runStep(h, m, st); e != nil {
			if !st.Skip {
				return fmt.Errorf("%s: %w", st.Name, e)
			}
			m.log("Optional stage failed, continuing: " + e.Error())
		}
	}
	return nil
}
func runStep(h *Host, m *Managed, st Step) error {
	at := func(rel string) (string, error) { return inside(m.root, rel) }
	exists := func(p string) bool { _, e := os.Stat(p); return e == nil }
	switch st.Type {
	case "steam":
		if h == nil {
			return errors.New("SteamCMD cannot run before start")
		}
		dest, e := at(st.Target)
		if e != nil {
			return e
		}
		return steamInstall(h, m, st.AppID, dest, st.Branch, st.Platform)
	case "createdirectory":
		p, e := at(st.Args)
		if e != nil {
			return e
		}
		return os.MkdirAll(p, 0700)
	case "createfile":
		p, e := at(st.Args)
		if e != nil || (exists(p) && !st.Overwrite) {
			return e
		}
		os.MkdirAll(filepath.Dir(p), 0700)
		return atomicWrite(p, []byte(st.Data), 0600)
	case "copyfilepath":
		src, e := at(st.Data)
		if e != nil {
			return e
		}
		dst, e := at(st.Args)
		if e != nil || (exists(dst) && !st.Overwrite) {
			return e
		}
		if fi, e := os.Stat(src); e != nil {
			return e
		} else if fi.IsDir() {
			return copyTree(src, dst)
		}
		b, e := os.ReadFile(src)
		if e != nil {
			return e
		}
		os.MkdirAll(filepath.Dir(dst), 0700)
		return atomicWrite(dst, b, 0700)
	case "setexecutableflag":
		p, e := at(st.Args)
		if e != nil {
			return e
		}
		return os.Chmod(p, 0755)
	case "pause":
		n, _ := strconv.Atoi(st.Args)
		time.Sleep(time.Duration(min(max(n, 0), 60)) * time.Second)
		return nil
	case "startapplication", "waitforstartupcomplete", "shutdownapplication":
		// AMP uses these to generate default config on first boot; the game does that itself on first start.
		return nil
	case "extractarchive":
		src, e := at(st.Data)
		if e != nil {
			return e
		}
		dst, e := at(st.Target)
		if e != nil {
			return e
		}
		if e = extract(src, dst); e != nil {
			return e
		}
		os.Remove(src)
		return nil
	case "fetchurl", "fetchurlfromjq", "githubrelease":
		return fetchStage(h, m, st)
	case "executable":
		cmd := stageCommand(st.Data, strings.ReplaceAll(st.Args, "$ROOT", m.root))
		cmd.Dir = filepath.Join(m.root, "game")
		os.MkdirAll(cmd.Dir, 0700)
		f, e := os.OpenFile(filepath.Join(m.root, "console.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
		return cmd.Run()
	}
	return errors.New("unsupported installation stage " + st.Type)
}

// fetchStage downloads a URL (direct, from a JSON document, or a GitHub release asset) into Target.
func fetchStage(h *Host, m *Managed, st Step) error {
	raw, name := st.Data, st.Args
	switch st.Type {
	case "fetchurlfromjq":
		var doc any
		if e := fetchJSON(st.Data, &doc); e != nil {
			return e
		}
		v, e := jsonPath(doc, st.Args)
		if e != nil {
			return e
		}
		raw, name = v, ""
	case "githubrelease":
		api := "https://api.github.com/repos/" + st.Args + "/releases/latest"
		if st.Branch != "" && st.Branch != "latest" {
			api = "https://api.github.com/repos/" + st.Args + "/releases/tags/" + url.PathEscape(st.Branch)
		}
		var rel struct {
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if e := fetchJSON(api, &rel); e != nil {
			return e
		}
		raw, name = "", st.Data
		for _, a := range rel.Assets {
			if a.Name == st.Data || (st.Data == "" && raw == "") {
				raw = a.URL
				if name == "" {
					name = a.Name
				}
			}
		}
		if raw == "" {
			return fmt.Errorf("release asset %q not found in %s", st.Data, st.Args)
		}
	}
	u, e := url.Parse(raw)
	if e != nil {
		return e
	}
	if name == "" {
		name = path.Base(u.Path)
	}
	if name == "" || name == "/" || name == "." || strings.ContainsAny(name, `/\`) {
		return errors.New("cannot determine download file name")
	}
	dir, e := inside(m.root, st.Target)
	if e != nil {
		return e
	}
	dest := filepath.Join(dir, name)
	if !st.Extract && !st.Overwrite {
		if _, e := os.Stat(dest); e == nil {
			return nil
		}
	}
	downloads := filepath.Join(m.root, "downloads")
	if h != nil {
		downloads = filepath.Join(h.dir, "downloads")
	}
	tmp := filepath.Join(downloads, token(8)+"-"+name)
	defer os.Remove(tmp)
	m.log("Downloading " + u.Host + u.Path)
	if e = download(raw, tmp, "", m.log); e != nil {
		return e
	}
	if st.Extract {
		return extract(tmp, dir)
	}
	os.MkdirAll(dir, 0700)
	return replaceFile(tmp, dest)
}

// jsonPath resolves the simple paths AMP templates use, e.g. $.assets[1].browser_download_url.
func jsonPath(doc any, p string) (string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "$")
	p = strings.NewReplacer(".[", ".[", "[", ".[").Replace(p)
	cur := doc
	for _, part := range strings.Split(p, ".") {
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
			i, e := strconv.Atoi(part[1 : len(part)-1])
			a, ok := cur.([]any)
			if e != nil || !ok || i < 0 || i >= len(a) {
				return "", fmt.Errorf("JSON path %s not found", p)
			}
			cur = a[i]
			continue
		}
		o, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("JSON path %s not found", p)
		}
		cur = o[part]
	}
	if s, ok := cur.(string); ok && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("JSON path %s is not a URL", p)
}
