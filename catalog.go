package main

import (
	"archive/tar"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/amp/* assets/amp-source.json assets/playit-release.json
var embeddedAssets embed.FS

type Catalog struct {
	mu        sync.RWMutex
	items     map[string]Template
	root      string
	revision  string
	refreshed time.Time
}

var includeRE = regexp.MustCompile(`(?i)^@includejson\[([^\]]+)\]$`)

// Vanilla defaults minus the keys LANForge manages (server-port, max-players, level-name).
const minecraftProperties = `motd=A LANForge Minecraft server
gamemode=survival
difficulty=easy
hardcore=false
pvp=true
online-mode=true
white-list=false
enforce-whitelist=false
allow-flight=false
spawn-monsters=true
spawn-protection=16
view-distance=10
simulation-distance=10
level-seed=
level-type=minecraft\:normal
generate-structures=true
generator-settings={}
max-world-size=29999984
force-gamemode=false
enable-command-block=false
player-idle-timeout=0
op-permission-level=4
function-permission-level=2
pause-when-empty-seconds=60
hide-online-players=false
enforce-secure-profile=true
prevent-proxy-connections=false
enable-status=true
enable-query=false
enable-rcon=false
broadcast-console-to-ops=true
broadcast-rcon-to-ops=true
log-ips=true
max-tick-time=60000
max-chained-neighbor-updates=1000000
network-compression-threshold=256
rate-limit=0
sync-chunk-writes=true
use-native-transport=true
accepts-transfers=false
require-resource-pack=false
resource-pack=
resource-pack-sha1=
resource-pack-prompt=
initial-enabled-packs=vanilla
initial-disabled-packs=
`

var trailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)
var variableRE = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

func parseKVP(b []byte) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(l, "\ufeff"))
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			k = strings.TrimSpace(k)
			m[k] = strings.TrimPrefix(v, k+"=") // tolerate "Key=Key=value" typos
		}
	}
	return m
}
func newCatalog(data string) (*Catalog, error) {
	c := &Catalog{root: filepath.Join(data, "catalog"), items: map[string]Template{}}
	if _, e := os.Stat(filepath.Join(c.root, "manifest.json")); e != nil {
		if e = os.MkdirAll(c.root, 0700); e != nil {
			return nil, e
		}
		es, _ := fs.ReadDir(embeddedAssets, "assets/amp")
		for _, f := range es {
			b, e := embeddedAssets.ReadFile("assets/amp/" + f.Name())
			if e != nil {
				return nil, e
			}
			if e = atomicWrite(filepath.Join(c.root, f.Name()), b, 0600); e != nil {
				return nil, e
			}
		}
		b, _ := embeddedAssets.ReadFile("assets/amp-source.json")
		atomicWrite(filepath.Join(c.root, "source.json"), b, 0600)
	}
	if e := c.load(); e != nil {
		return nil, e
	}
	return c, nil
}
func (c *Catalog) load() error {
	var source struct {
		Commit string `json:"commit"`
	}
	readJSON(filepath.Join(c.root, "source.json"), &source)
	items := map[string]Template{}
	pretty := func(p string) string { return strings.ToUpper(p[:1]) + p[1:] }
	files, e := os.ReadDir(c.root)
	if e != nil {
		return e
	}
	read := func(name string) ([]byte, error) {
		p, e := inside(c.root, name)
		if e != nil {
			return nil, e
		}
		b, e := os.ReadFile(p)
		if strings.HasSuffix(name, ".json") {
			// Some community templates have trailing commas, which encoding/json rejects.
			b = trailingCommaRE.ReplaceAll(b, []byte("$1"))
		}
		return b, e
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".kvp") {
			continue
		}
		b, e := read(f.Name())
		if e != nil {
			continue
		}
		kv := parseKVP(b)
		id := strings.TrimSuffix(f.Name(), ".kvp")
		t := Template{ID: id, Name: kv["Meta.DisplayName"], Description: kv["Meta.Description"], Engine: "amp", Status: "needs_setup", Reason: "Template requires compatibility review", Source: "AMP Community", Revision: source.Commit, KVP: kv, Fields: []Field{}, Ports: []Port{}, Files: map[string]string{}}
		if t.Name == "" {
			t.Name = id
		}
		for _, s := range strings.Split(kv["Meta.OS"], ",") {
			t.OS = append(t.OS, strings.ToLower(strings.TrimSpace(s)))
		}
		resolve := func(v string, out any) error {
			if m := includeRE.FindStringSubmatch(v); m != nil {
				b, e := read(m[1])
				if e != nil {
					return e
				}
				return json.Unmarshal(b, out)
			}
			return json.Unmarshal([]byte(v), out)
		}
		var ports []map[string]any
		if resolve(kv["App.Ports"], &ports) == nil {
			t.Ports = parsePorts(ports)
		}
		var fields []map[string]any
		if b, e := read(kv["Meta.ConfigManifest"]); e == nil && json.Unmarshal(b, &fields) == nil {
			t.RawFields = fields
			for _, f := range fields {
				if str(f["FieldName"]) == "" {
					continue
				}
				typ := "text"
				switch strings.ToLower(str(f["InputType"])) {
				case "number":
					typ = "number"
				case "password":
					typ = "password"
				case "enum", "dropdown":
					typ = "select"
				case "checkbox":
					typ = "select"
				}
				opts := map[string]string{}
				if m, ok := f["EnumValues"].(map[string]any); ok {
					for k, v := range m {
						if strings.EqualFold(str(f["InputType"]), "checkbox") {
							opts[str(v)] = k
						} else {
							opts[k] = str(v)
						}
					}
				}
				t.Fields = append(t.Fields, Field{Hidden: asBool(f["Hidden"]), ID: str(f["FieldName"]), Name: str(f["DisplayName"]), Description: str(f["Description"]), Default: str(f["DefaultValue"]), Type: typ, Options: opts})
			}
		}
		resolve(kv["App.UpdateSources"], &t.Updates)
		t.SteamApp = steamApp(kv, t.Updates)
		if v := kv["App.PreStartStages"]; v != "" {
			resolve(v, &t.PreStart)
		}
		if n := kv["Meta.MetaConfigManifest"]; n != "" {
			if b, e := read(n); e == nil {
				json.Unmarshal(b, &t.Meta)
			}
		}
		// Only list platforms whose plan actually compiles; the UI uses OS to pick hosts.
		declared, reasons := t.OS, []string{}
		t.OS = []string{}
		for _, plat := range declared {
			if plat == "" {
				continue
			}
			reason := genericCompatibility(t, plat)
			if reason == "" {
				if _, e := compilePlan(t, Server{Ports: t.Ports, Settings: map[string]string{}}, plat); e != nil {
					reason = e.Error()
				}
			}
			if reason == "" {
				t.OS = append(t.OS, plat)
			} else {
				reasons = append(reasons, pretty(plat)+": "+reason)
			}
		}
		if len(t.OS) > 0 {
			t.Status = "experimental"
			t.Reason = "AMP community installer. Not game-launch verified."
			if !asBool(kv["App.SteamUpdateAnonymousLogin"]) && strings.Contains(kv["App.UpdateSources"]+fmt.Sprint(t.Updates), "SteamCMD") {
				t.Reason = "Needs Steam sign-in (Computers page) with an account that owns the game. " + t.Reason
			}
			if len(reasons) > 0 {
				t.Reason += " " + strings.Join(reasons, " · ")
			}
		} else {
			t.OS = declared
			t.Status = "needs_setup"
			t.Reason = strings.Join(reasons, " · ")
			if t.Reason == "" {
				t.Reason = "Template declares no supported operating system"
			}
		}
		items[id] = t
	}
	items["minecraft-java"] = Template{ID: "minecraft-java", Name: "Minecraft Java", Description: "Vanilla, Paper, Fabric, Forge and more, with Modrinth mods and modpacks. Java is installed automatically.", OS: []string{"windows", "linux"}, Engine: "minecraft", Status: "supported", Source: "Mojang", Revision: "builtin-1", Ports: []Port{{"game", "Game", 25565, "tcp", true}}, Fields: []Field{{ID: "version", Name: "Version", Default: "latest", Type: "text"}, {ID: "loader", Name: "Server type", Description: "Paper and Purpur run Spigot/Bukkit plugins. Fabric, Quilt, Forge and NeoForge run mods. Changing this later needs Install / repair.", Default: "vanilla", Type: "select", Options: map[string]string{"vanilla": "Vanilla", "paper": "Paper (Spigot plugins)", "purpur": "Purpur (Spigot plugins)", "fabric": "Fabric (mods)", "quilt": "Quilt (mods)", "forge": "Forge (mods)", "neoforge": "NeoForge (mods)"}}, {ID: "loaderVersion", Name: "Loader version", Description: "latest picks the newest stable build for your Minecraft version.", Default: "latest", Type: "text"}, {ID: "modpack", Name: "Modrinth modpack (optional)", Description: "Paste a modrinth.com/modpack link. It sets the Minecraft version and loader for you.", Default: "", Type: "text"}, {ID: "mods", Name: "Modrinth mods", Description: "Modrinth project IDs to install, separated by commas.", Default: "", Type: "text", Hidden: true}, {ID: "memory", Name: "Maximum memory (MB)", Default: "2048", Type: "number"}, {ID: "maxPlayers", Name: "Player limit", Default: "20", Type: "number"}, {ID: "world", Name: "World name", Default: "world", Type: "text"}, {ID: "eula", Name: "I accept the Minecraft EULA", Default: "false", Type: "checkbox"}, {ID: "properties", Name: "server.properties", Description: "Every vanilla setting, one key=value per line. Port, player limit and world name come from the settings above.", Default: minecraftProperties, Type: "textarea"}, {ID: "whitelist", Name: "Whitelist", Description: "One player name per line. Turns the whitelist on. Leave empty to let anyone join.", Default: "", Type: "textarea"}}}
	items["app"] = Template{ID: "app", Name: "Custom app (Node.js, Python, Go…)", Description: "Run your own website, Discord bot, API or game server written in Node.js, Python, Go, Java, Bun or Deno. The runtime is installed for you.", OS: []string{"windows", "linux"}, Engine: "app", Status: "supported", Source: "LANForge", Revision: "builtin-1", Ports: []Port{{"app", "App", 8080, "tcp", true}}, Fields: []Field{
		{ID: "runtime", Name: "Language", Description: "Changing this later needs Install / repair.", Default: "node", Type: "select", Options: map[string]string{"node": "Node.js / JavaScript", "python": "Python", "go": "Go", "java": "Java", "bun": "Bun", "deno": "Deno", "custom": "Custom executable"}},
		{ID: "source", Name: "Code from", Description: "Upload: after installing, upload a .zip in Files and click Extract, then Install / repair. Empty: write your files in Files.", Default: "empty", Type: "select", Options: map[string]string{"github": "GitHub repository", "upload": "Upload a zip", "empty": "Empty folder"}},
		{ID: "repo", Name: "GitHub repository", Description: "Public repo link, e.g. https://github.com/owner/project", Default: "", Type: "text"},
		{ID: "branch", Name: "Branch", Description: "Blank uses the default branch.", Default: "", Type: "text"},
		{ID: "start", Name: "Start command", Description: "Blank picks one: npm start, python main.py, ./app (Go), java -jar app.jar. Your app gets its port in the PORT environment variable.", Default: "", Type: "text"},
	}}
	if t, ok := items["valheim"]; ok {
		t.Engine = "valheim"
		t.Status = "supported"
		t.Reason = "Vanilla adapter. Mods are not included; host validation required."
		t.Description = "Vanilla Valheim dedicated server, installed through SteamCMD."
		t.Ports = []Port{{"game", "Game", 2456, "udp", true}, {"query", "Query", 2457, "udp", true}}
		t.Fields = []Field{{ID: "world", Name: "World name", Default: "Dedicated", Type: "text"}, {ID: "password", Name: "Server password (at least 5 characters)", Default: "", Type: "password"}, {ID: "public", Name: "List in server browser", Default: "1", Type: "select", Options: map[string]string{"1": "Yes", "0": "No"}}}
		items["valheim"] = t
	}
	if t, ok := items["terraria"]; ok {
		t.Engine = "terraria"
		t.SteamApp = "105600"
		t.Status = "supported"
		t.Reason = "Official standalone adapter. Windows console input needs host validation."
		t.Description = "Official dedicated server with automatic world creation."
		t.Ports = []Port{{"game", "Game", 7777, "tcp", true}}
		t.Fields = []Field{{ID: "world", Name: "World name", Default: "MyWorld", Type: "text"}, {ID: "size", Name: "World size", Default: "2", Type: "select", Options: map[string]string{"1": "Small", "2": "Medium", "3": "Large"}}, {ID: "maxPlayers", Name: "Player limit", Default: "8", Type: "number"}, {ID: "password", Name: "Password (optional)", Default: "", Type: "password"}}
		items["terraria"] = t
	}
	c.mu.Lock()
	c.items = items
	c.revision = source.Commit
	c.refreshed = time.Now()
	c.mu.Unlock()
	return nil
}
func parsePorts(rows []map[string]any) []Port {
	out := []Port{}
	var visit func([]map[string]any, int)
	visit = func(rows []map[string]any, parent int) {
		for _, p := range rows {
			n, _ := strconv.Atoi(str(p["Port"]))
			if parent > 0 {
				if v, ok := p["Offset"]; ok {
					o, _ := strconv.Atoi(str(v))
					n = parent + o
				}
			}
			proto := strings.ToLower(str(p["Protocol"]))
			if proto == "" {
				proto = "tcp"
			}
			count := 1
			if x, e := strconv.Atoi(str(p["Range"])); e == nil && x > 1 && x < 100 {
				count = x
			}
			for i := 0; i < count; i++ {
				id := str(p["Ref"])
				if i > 0 {
					id += fmt.Sprint("_", i)
				}
				out = append(out, Port{id, str(p["Name"]), n + i, proto, !strings.Contains(strings.ToLower(str(p["Name"])), "rcon") && !strings.Contains(strings.ToLower(str(p["Name"])), "admin")})
			}
			if a, ok := p["ChildPorts"].([]any); ok {
				children := []map[string]any{}
				for _, v := range a {
					if m, ok := v.(map[string]any); ok {
						children = append(children, m)
					}
				}
				visit(children, n)
			}
		}
	}
	visit(rows, 0)
	return out
}
func (c *Catalog) list() []Template {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a := []Template{}
	for _, t := range c.items {
		a = append(a, t)
	}
	sort.Slice(a, func(i, j int) bool {
		if (a[i].Status == "supported") != (a[j].Status == "supported") {
			return a[i].Status == "supported"
		}
		return a[i].Name < a[j].Name
	})
	return a
}
func (c *Catalog) get(id string) (Template, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.items[id]
	if !ok {
		return t, errors.New("unknown game template")
	}
	return t, nil
}
func (c *Catalog) refresh() error {
	var commit struct {
		SHA string `json:"sha"`
	}
	if e := fetchJSON("https://api.github.com/repos/CubeCoders/AMPTemplates/commits/main", &commit); e != nil {
		return e
	}
	if len(commit.SHA) != 40 {
		return errors.New("invalid upstream revision")
	}
	tmp, e := os.MkdirTemp(filepath.Dir(c.root), "catalog-stage-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "amp.tar.gz")
	if e = download("https://codeload.github.com/CubeCoders/AMPTemplates/tar.gz/"+commit.SHA, archive, "", nil); e != nil {
		return e
	}
	f, e := os.Open(archive)
	if e != nil {
		return e
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	stage := filepath.Join(tmp, "files")
	os.MkdirAll(stage, 0700)
	count := 0
	var total int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		_, name, ok := strings.Cut(h.Name, "/")
		if !ok || name == "" {
			continue
		}
		if h.Size > 5<<20 || total+h.Size > 100<<20 {
			return errors.New("template catalog too large")
		}
		total += h.Size
		p, e := inside(stage, name)
		if e != nil {
			return e
		}
		b, e := io.ReadAll(io.LimitReader(tr, 5<<20))
		if e != nil {
			return e
		}
		if e = atomicWrite(p, b, 0600); e != nil {
			return e
		}
		if strings.HasSuffix(name, ".kvp") {
			count++
		}
	}
	if count < 10 {
		return errors.New("incomplete template catalog; keeping previous version")
	}
	writeJSON(filepath.Join(stage, "source.json"), map[string]string{"commit": commit.SHA, "repository": "https://github.com/CubeCoders/AMPTemplates"})
	// Keep old snapshot for rollback. No running instance uses these mutable files.
	old := c.root + "-" + token(4)
	if e = os.Rename(c.root, old); e != nil {
		return e
	}
	if e = os.Rename(stage, c.root); e != nil {
		os.Rename(old, c.root)
		return e
	}
	return c.load()
}
func splitArgs(s string) ([]string, error) {
	out := []string{}
	var b strings.Builder
	quote := rune(0)
	active := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\'') {
			b.WriteRune(runes[i+1])
			i++
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			active = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			active = true
		} else if r == ' ' || r == '\t' || r == '\n' {
			if active {
				out = append(out, b.String())
				b.Reset()
				active = false
			}
		} else {
			b.WriteRune(r)
			active = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote in launch arguments")
	}
	if active {
		out = append(out, b.String())
	}
	return out, nil
}
