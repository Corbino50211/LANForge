package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// App servers run the user's own program with a runtime LANForge installs inside the server folder.

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

func installApp(m *Managed) error {
	s := m.specCopy()
	switch s.Settings["source"] {
	case "github":
		if e := fetchGitHubSource(m, s.Settings["repo"], s.Settings["branch"]); e != nil {
			return e
		}
	case "upload", "empty", "":
		os.MkdirAll(filepath.Join(m.root, "game"), 0700)
	}
	rt := s.Settings["runtime"]
	if rt == "custom" {
		return nil
	}
	if _, e := installRuntime(m, rt); e != nil {
		return e
	}
	return installAppDeps(m, rt)
}

var githubRepoRE = regexp.MustCompile(`^(?:https?://)?(?:www\.)?github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// fetchGitHubSource downloads a public repository without git and copies it into the game folder.
func fetchGitHubSource(m *Managed, repo, branch string) error {
	match := githubRepoRE.FindStringSubmatch(strings.TrimSpace(repo))
	if match == nil {
		return errors.New("enter a GitHub repository link like https://github.com/owner/project")
	}
	u := "https://api.github.com/repos/" + match[1] + "/" + match[2] + "/zipball"
	if branch = strings.TrimSpace(branch); branch != "" {
		u += "/" + url.PathEscape(branch)
	}
	archive := filepath.Join(m.root, "downloads", "source.zip")
	defer os.Remove(archive)
	m.log("Downloading " + match[1] + "/" + match[2] + " from GitHub")
	if e := download(u, archive, "", m.log); e != nil {
		return fmt.Errorf("could not download the repository (is it public?): %w", e)
	}
	return extractFlat(archive, filepath.Join(m.root, "game"))
}

// extractFlat extracts an archive into dest, dropping a single top-level folder such as project-main/.
func extractFlat(archive, dest string) error {
	tmp, e := os.MkdirTemp(filepath.Dir(archive), ".extract-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	if e = extract(archive, tmp); e != nil {
		return e
	}
	src := tmp
	if es, _ := os.ReadDir(tmp); len(es) == 1 && es[0].IsDir() {
		src = filepath.Join(tmp, es[0].Name())
	}
	os.MkdirAll(dest, 0700)
	return copyTree(src, dest)
}

type githubRelease struct {
	Assets []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func githubAsset(repo, release string, match func(string) bool) (string, string, error) {
	var r githubRelease
	if e := fetchJSON("https://api.github.com/repos/"+repo+"/releases/"+release, &r); e != nil {
		return "", "", e
	}
	for _, a := range r.Assets {
		if match(a.Name) {
			return a.URL, a.Digest, nil
		}
	}
	return "", "", errors.New("no download for this platform in " + repo)
}

// installRuntime installs a language runtime for this server and returns its main binary relative to the server root.
func installRuntime(m *Managed, rt string) (string, error) {
	if rt == "java" {
		return installJava(m, 21)
	}
	marker := filepath.Join(m.root, "runtime-"+rt+".json")
	if rel := runtimeBinary(m.root, rt); rel != "" {
		return rel, nil
	}
	win := runtime.GOOS == "windows"
	var u, sum, bin string
	var e error
	switch rt {
	case "node":
		var idx []struct {
			Version string `json:"version"`
			LTS     any    `json:"lts"`
		}
		if e = fetchJSON("https://nodejs.org/dist/index.json", &idx); e != nil {
			return "", e
		}
		v := ""
		for _, x := range idx {
			if x.LTS != false && x.LTS != nil {
				v = x.Version
				break
			}
		}
		name := "node-" + v + "-linux-x64.tar.gz"
		if win {
			name = "node-" + v + "-win-x64.zip"
		}
		u = "https://nodejs.org/dist/" + v + "/" + name
		sums, e := fetchBytes("https://nodejs.org/dist/" + v + "/SHASUMS256.txt")
		if e != nil {
			return "", e
		}
		for _, l := range strings.Split(string(sums), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[1] == name {
				sum = f[0]
			}
		}
		bin = exeName("node")
	case "python":
		var latest struct {
			Tag string `json:"tag"`
		}
		if e = fetchJSON("https://raw.githubusercontent.com/astral-sh/python-build-standalone/latest-release/latest-release.json", &latest); e != nil {
			return "", e
		}
		suffix := "-x86_64-unknown-linux-gnu-install_only.tar.gz"
		bin = "python3"
		if win {
			suffix, bin = "-x86_64-pc-windows-msvc-install_only.tar.gz", "python.exe"
		}
		// 3.12 has the widest package support.
		u, sum, e = githubAsset("astral-sh/python-build-standalone", "tags/"+latest.Tag, func(n string) bool {
			return strings.HasPrefix(n, "cpython-3.12.") && strings.HasSuffix(n, suffix)
		})
	case "go":
		var rel []struct {
			Stable bool `json:"stable"`
			Files  []struct {
				Filename string `json:"filename"`
				OS       string `json:"os"`
				Arch     string `json:"arch"`
				Kind     string `json:"kind"`
				SHA256   string `json:"sha256"`
			} `json:"files"`
		}
		if e = fetchJSON("https://go.dev/dl/?mode=json", &rel); e != nil {
			return "", e
		}
		for _, r := range rel {
			for _, f := range r.Files {
				if u == "" && r.Stable && f.OS == runtime.GOOS && f.Arch == "amd64" && f.Kind == "archive" {
					u, sum = "https://go.dev/dl/"+f.Filename, f.SHA256
				}
			}
		}
		bin = exeName("go")
	case "bun":
		want := "bun-linux-x64.zip"
		if win {
			want = "bun-windows-x64.zip"
		}
		u, sum, e = githubAsset("oven-sh/bun", "latest", func(n string) bool { return n == want })
		bin = exeName("bun")
	case "deno":
		want := "deno-x86_64-unknown-linux-gnu.zip"
		if win {
			want = "deno-x86_64-pc-windows-msvc.zip"
		}
		u, sum, e = githubAsset("denoland/deno", "latest", func(n string) bool { return n == want })
		bin = exeName("deno")
	default:
		return "", errors.New("unknown runtime " + rt)
	}
	if e != nil {
		return "", e
	}
	if u == "" {
		return "", errors.New("no " + rt + " download for this platform")
	}
	archive := filepath.Join(m.root, "downloads", path.Base(u))
	defer os.Remove(archive)
	m.log("Installing " + rt + " for this server")
	if e = download(u, archive, sum, m.log); e != nil {
		return "", e
	}
	dest := filepath.Join(m.root, "runtime-"+rt)
	os.RemoveAll(dest)
	if e = extract(archive, dest); e != nil {
		return "", e
	}
	found := findFile(dest, bin)
	if found == "" {
		return "", fmt.Errorf("%s archive did not contain %s", rt, bin)
	}
	os.Chmod(found, 0755)
	rel, _ := filepath.Rel(m.root, found)
	rel = filepath.ToSlash(rel)
	return rel, writeJSON(marker, rel)
}

// runtimeBinary returns the installed runtime's binary for this server, or "".
func runtimeBinary(root, rt string) string {
	marker := filepath.Join(root, "runtime-"+rt+".json")
	if rt == "java" {
		marker = filepath.Join(root, "java-21.json")
	}
	var rel string
	if readJSON(marker, &rel) == nil {
		if p, e := inside(root, rel); e == nil {
			if _, e = os.Stat(p); e == nil {
				return rel
			}
		}
	}
	return ""
}

// findFile returns the shallowest regular file with this name under dir.
func findFile(dir, name string) string {
	found := ""
	filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && d.Name() == name && (found == "" || len(p) < len(found)) {
			found = p
		}
		return nil
	})
	return found
}

func appEnv(root, rt string) []string {
	bin := runtimeBinary(root, rt)
	env := []string{}
	if bin != "" {
		env = append(env, "PATH="+filepath.Join(root, filepath.Dir(bin))+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if rt == "go" {
		env = append(env, "GOPATH="+filepath.Join(root, "gopath"))
		if bin != "" {
			env = append(env, "GOROOT="+filepath.Join(root, filepath.Dir(filepath.Dir(bin))))
		}
	}
	return env
}

func installAppDeps(m *Managed, rt string) error {
	game := filepath.Join(m.root, "game")
	has := func(n string) bool { _, e := os.Stat(filepath.Join(game, n)); return e == nil }
	bin := filepath.Join(m.root, runtimeBinary(m.root, rt))
	env := appEnv(m.root, rt)
	var e error
	switch {
	case rt == "node" && has("package.json"):
		m.log("Installing npm packages")
		e = runLogged(m, game, env, bin, findFile(filepath.Join(m.root, "runtime-node"), "npm-cli.js"), "install")
	case rt == "bun" && has("package.json"):
		m.log("Installing packages with Bun")
		e = runLogged(m, game, env, bin, "install")
	case rt == "python" && has("requirements.txt"):
		m.log("Installing Python packages")
		e = runLogged(m, game, env, bin, "-m", "pip", "install", "-r", "requirements.txt")
	case rt == "go" && has("go.mod"):
		m.log("Building the Go program")
		e = runLogged(m, game, env, bin, "build", "-o", exeName("app"), ".")
	}
	if e != nil {
		return fmt.Errorf("installing dependencies failed: %w. Check the console", e)
	}
	return nil
}

// defaultStart guesses the start command from the project files.
func defaultStart(rt, game string) (string, error) {
	has := func(n string) bool { _, e := os.Stat(filepath.Join(game, n)); return e == nil }
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	readJSON(filepath.Join(game, "package.json"), &pkg)
	switch rt {
	case "node":
		if pkg.Scripts["start"] != "" {
			return "npm start", nil
		}
		return "node index.js", nil
	case "bun":
		if pkg.Scripts["start"] != "" {
			return "bun run start", nil
		}
		return "bun run index.ts", nil
	case "python":
		if !has("main.py") && has("app.py") {
			return "python app.py", nil
		}
		return "python main.py", nil
	case "go":
		return "./app", nil
	case "java":
		if jars, _ := filepath.Glob(filepath.Join(game, "*.jar")); len(jars) == 1 {
			return "java -jar " + filepath.Base(jars[0]), nil
		}
		return "java -jar app.jar", nil
	case "deno":
		return "deno run -A main.ts", nil
	}
	return "", errors.New("set a start command in Settings, e.g. ./myserver")
}

// splitCommand splits a command line on spaces, honouring single and double quotes.
func splitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	quote, inWord := rune(0), false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("start command has an unclosed quote")
	}
	if inWord {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, errors.New("start command is empty")
	}
	return out, nil
}

// appPlan turns the start command into a launch plan, mapping runtime tools to the bundled binaries.
func appPlan(s Server, root string) (*Plan, error) {
	rt, game := s.Settings["runtime"], filepath.Join(root, "game")
	start := strings.TrimSpace(s.Settings["start"])
	if start == "" {
		var e error
		if start, e = defaultStart(rt, game); e != nil {
			return nil, e
		}
	}
	words, e := splitCommand(start)
	if e != nil {
		return nil, e
	}
	tool, args := words[0], words[1:]
	bin := runtimeBinary(root, rt)
	rootRel := func(p string) string { r, _ := filepath.Rel(root, p); return "$ROOT/" + filepath.ToSlash(r) }
	exe := ""
	switch {
	case bin != "" && (tool == "node" && rt == "node" || (tool == "python" || tool == "python3") && rt == "python" || tool == "go" && rt == "go" || tool == "java" && rt == "java" || tool == "bun" && rt == "bun" || tool == "deno" && rt == "deno"):
		exe = bin
	case bin != "" && rt == "node" && (tool == "npm" || tool == "npx"):
		cli := findFile(filepath.Join(root, "runtime-node"), tool+"-cli.js")
		if cli == "" {
			return nil, errors.New(tool + " is missing from the Node.js runtime; use Install / repair")
		}
		exe, args = bin, append([]string{rootRel(cli)}, args...)
	case bin != "" && rt == "python" && (tool == "pip" || tool == "pip3"):
		exe, args = bin, append([]string{"-m", "pip"}, args...)
	case bin != "" && rt == "bun" && tool == "bunx":
		exe, args = bin, append([]string{"x"}, args...)
	default:
		p := strings.TrimPrefix(filepath.ToSlash(tool), "./")
		if runtime.GOOS == "windows" && filepath.Ext(p) == "" {
			if _, e := os.Stat(filepath.Join(game, p+".exe")); e == nil {
				p += ".exe"
			}
		}
		if _, e := inside(game, p); e != nil {
			return nil, fmt.Errorf("start command must run a file inside the app folder or a %s tool", rt)
		}
		exe = "game/" + p
		os.Chmod(filepath.Join(game, p), 0755)
	}
	env := map[string]string{}
	if len(s.Ports) > 0 {
		env["PORT"] = strconv.Itoa(s.Ports[0].Number)
	}
	if bin != "" {
		sep := ":"
		if runtime.GOOS == "windows" {
			sep = ";"
		}
		env["PATH"] = "$ROOT/" + path.Dir(bin) + sep + "%PATH%"
	}
	if rt == "go" {
		env["GOPATH"] = "$ROOT/gopath"
		if bin != "" {
			env["GOROOT"] = "$ROOT/" + path.Dir(path.Dir(bin))
		}
	}
	return &Plan{Engine: "app", Executable: exe, Args: args, WorkDir: "game", Env: env}, nil
}
