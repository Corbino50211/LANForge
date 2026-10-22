package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const modrinthAPI = "https://api.modrinth.com/v2"

// modTarget says where a server's add-ons go and which Modrinth loaders fit it.
func modTarget(s Server) (folder, projectType string, loaders []string, mc string, e error) {
	loader := s.Settings["installedLoader"]
	if loader == "" {
		loader = s.Settings["loader"]
	}
	mc = s.Settings["installedVersion"]
	if s.Engine != "minecraft" || mc == "" {
		return "", "", nil, "", errors.New("install the server first")
	}
	switch loader {
	case "paper", "purpur":
		return "plugins", "plugin", []string{"paper", "spigot", "bukkit", "purpur"}, mc, nil
	case "quilt":
		return "mods", "mod", []string{"quilt", "fabric"}, mc, nil
	case "fabric", "forge", "neoforge":
		return "mods", "mod", []string{loader}, mc, nil
	}
	return "", "", nil, "", errors.New("vanilla servers can't load mods; create the server with Fabric, Forge, NeoForge, Quilt, Paper or Purpur")
}

func quoteList(v []string) string { b, _ := json.Marshal(v); return string(b) }

func modsSearch(m *Managed, q string) (any, error) {
	_, typ, loaders, mc, e := modTarget(m.specCopy())
	if e != nil {
		return nil, e
	}
	cats := []string{}
	for _, l := range loaders {
		cats = append(cats, "categories:"+l)
	}
	facets, _ := json.Marshal([][]string{{"project_type:" + typ}, cats, {"versions:" + mc}, {"server_side:required", "server_side:optional"}})
	var res struct {
		Hits []struct {
			ID          string `json:"project_id"`
			Slug        string `json:"slug"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Icon        string `json:"icon_url"`
			Downloads   int    `json:"downloads"`
			Author      string `json:"author"`
		} `json:"hits"`
	}
	u := modrinthAPI + "/search?limit=20&index=relevance&query=" + url.QueryEscape(q) + "&facets=" + url.QueryEscape(string(facets))
	if e := fetchJSON(u, &res); e != nil {
		return nil, e
	}
	return res.Hits, nil
}

type modRecord struct {
	File    string `json:"file"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

func modRecords(game string) map[string]modRecord {
	r := map[string]modRecord{}
	readJSON(filepath.Join(game, ".lanforge-mods.json"), &r)
	return r
}

type modVersion struct {
	Number string `json:"version_number"`
	Files  []struct {
		URL      string            `json:"url"`
		Filename string            `json:"filename"`
		Primary  bool              `json:"primary"`
		Hashes   map[string]string `json:"hashes"`
	} `json:"files"`
	Dependencies []struct {
		ProjectID string `json:"project_id"`
		Type      string `json:"dependency_type"`
	} `json:"dependencies"`
}

// modsInstall installs a Modrinth project and its required dependencies.
func modsInstall(m *Managed, project string) (any, error) {
	if !m.op.TryLock() {
		return nil, errors.New("server is busy")
	}
	defer m.op.Unlock()
	if m.snapshot().PID != 0 {
		return nil, errors.New("stop the server before changing mods")
	}
	s := m.specCopy()
	folder, _, loaders, mc, e := modTarget(s)
	if e != nil {
		return nil, e
	}
	game := filepath.Join(m.root, "game")
	records := modRecords(game)
	installed := []string{}
	var install func(id string, depth int) error
	install = func(id string, depth int) error {
		if !safeID.MatchString(id) || depth > 8 {
			return errors.New("invalid Modrinth project")
		}
		var p struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if e := fetchJSON(modrinthAPI+"/project/"+id, &p); e != nil {
			return fmt.Errorf("Modrinth project %s: %w", id, e)
		}
		if _, ok := records[p.ID]; ok && depth > 0 {
			return nil // dependency already present
		}
		var versions []modVersion
		if e := fetchJSON(modrinthAPI+"/project/"+p.ID+"/version?loaders="+url.QueryEscape(quoteList(loaders))+"&game_versions="+url.QueryEscape(quoteList([]string{mc})), &versions); e != nil {
			return e
		}
		if len(versions) == 0 || len(versions[0].Files) == 0 {
			return fmt.Errorf("%s has no version for Minecraft %s on this loader", p.Title, mc)
		}
		v := versions[0]
		f := v.Files[0]
		for _, x := range v.Files {
			if x.Primary {
				f = x
			}
		}
		dest, e := inside(filepath.Join(game, folder), f.Filename)
		if e != nil || filepath.Dir(dest) != filepath.Join(game, folder) {
			return errors.New("invalid mod file name")
		}
		m.log("Installing " + p.Title + " " + v.Number)
		if e = download(f.URL, dest, f.Hashes["sha512"], nil); e != nil {
			return e
		}
		if old, ok := records[p.ID]; ok && old.File != f.Filename {
			os.Remove(filepath.Join(game, folder, old.File))
		}
		records[p.ID] = modRecord{File: f.Filename, Title: p.Title, Version: v.Number}
		installed = append(installed, p.Title)
		for _, d := range v.Dependencies {
			if d.Type == "required" && d.ProjectID != "" {
				if e := install(d.ProjectID, depth+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	e = install(project, 0)
	writeJSON(filepath.Join(game, ".lanforge-mods.json"), records)
	if e != nil {
		return nil, e
	}
	return map[string]string{"message": "Installed " + strings.Join(installed, ", ") + ". Start the server to load it."}, nil
}

func modsList(m *Managed) (any, error) {
	folder, _, _, _, e := modTarget(m.specCopy())
	if e != nil {
		return nil, e
	}
	game := filepath.Join(m.root, "game")
	byFile := map[string]modRecord{}
	for _, r := range modRecords(game) {
		byFile[r.File] = r
	}
	out := []modRecord{}
	jars, _ := filepath.Glob(filepath.Join(game, folder, "*.jar"))
	for _, j := range jars {
		r, ok := byFile[filepath.Base(j)]
		if !ok {
			r = modRecord{File: filepath.Base(j), Title: strings.TrimSuffix(filepath.Base(j), ".jar")}
		}
		out = append(out, r)
	}
	return map[string]any{"folder": folder, "mods": out}, nil
}

func modsRemove(m *Managed, file string) (any, error) {
	if !m.op.TryLock() {
		return nil, errors.New("server is busy")
	}
	defer m.op.Unlock()
	if m.snapshot().PID != 0 {
		return nil, errors.New("stop the server before changing mods")
	}
	folder, _, _, _, e := modTarget(m.specCopy())
	if e != nil {
		return nil, e
	}
	game := filepath.Join(m.root, "game")
	p, e := inside(filepath.Join(game, folder), file)
	if e != nil || !strings.HasSuffix(p, ".jar") || filepath.Dir(p) != filepath.Join(game, folder) {
		return nil, errors.New("invalid mod file")
	}
	if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	records := modRecords(game)
	for id, r := range records {
		if r.File == file {
			delete(records, id)
		}
	}
	return map[string]string{"message": "Removed " + file}, writeJSON(filepath.Join(game, ".lanforge-mods.json"), records)
}

// Modrinth modpacks (.mrpack): https://support.modrinth.com/en/articles/8802351
type mrIndex struct {
	Files []struct {
		Path      string            `json:"path"`
		Hashes    map[string]string `json:"hashes"`
		Env       map[string]string `json:"env"`
		Downloads []string          `json:"downloads"`
	} `json:"files"`
	Dependencies map[string]string `json:"dependencies"`
}
type modpack struct {
	Name, Archive string
	Index         mrIndex
}

var modpackRE = regexp.MustCompile(`^(?:https?://(?:www\.)?modrinth\.com/modpack/)?([A-Za-z0-9_-]+)(?:/.*)?$`)

func fetchModpack(m *Managed, ref, mc string) (*modpack, error) {
	match := modpackRE.FindStringSubmatch(strings.TrimSpace(ref))
	if match == nil {
		return nil, errors.New("enter a Modrinth modpack link or name, like modrinth.com/modpack/fabulously-optimized")
	}
	var p struct {
		ID, Title, ProjectType string
	}
	var raw map[string]any
	if e := fetchJSON(modrinthAPI+"/project/"+match[1], &raw); e != nil {
		return nil, fmt.Errorf("Modrinth modpack %s not found", match[1])
	}
	p.ID, p.Title, p.ProjectType = str(raw["id"]), str(raw["title"]), str(raw["project_type"])
	if p.ProjectType != "modpack" {
		return nil, fmt.Errorf("%s is a %s, not a modpack; add it from the Mods tab instead", p.Title, p.ProjectType)
	}
	u := modrinthAPI + "/project/" + p.ID + "/version"
	if mc != "" && mc != "latest" {
		u += "?game_versions=" + url.QueryEscape(quoteList([]string{mc}))
	}
	var versions []modVersion
	if e := fetchJSON(u, &versions); e != nil || len(versions) == 0 {
		return nil, fmt.Errorf("%s has no version for Minecraft %s", p.Title, mc)
	}
	f := versions[0].Files[0]
	for _, x := range versions[0].Files {
		if x.Primary {
			f = x
		}
	}
	archive := filepath.Join(m.root, "downloads", "modpack.zip") // .mrpack is a zip
	m.log("Downloading modpack " + p.Title + " " + versions[0].Number)
	if e := download(f.URL, archive, f.Hashes["sha512"], m.log); e != nil {
		return nil, e
	}
	z, e := zip.OpenReader(archive)
	if e != nil {
		return nil, e
	}
	defer z.Close()
	pack := &modpack{Name: p.Title, Archive: archive}
	for _, zf := range z.File {
		if zf.Name == "modrinth.index.json" {
			r, e := zf.Open()
			if e != nil {
				return nil, e
			}
			e = json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&pack.Index)
			r.Close()
			if e != nil {
				return nil, e
			}
		}
	}
	if pack.Index.Dependencies["minecraft"] == "" {
		return nil, errors.New("modpack has no modrinth.index.json")
	}
	return pack, nil
}

// applyModpack downloads the pack's server files and copies its overrides into the game folder.
func applyModpack(m *Managed, p *modpack) error {
	game := filepath.Join(m.root, "game")
	for i, f := range p.Index.Files {
		if f.Env["server"] == "unsupported" || len(f.Downloads) == 0 {
			continue
		}
		dest, e := inside(game, f.Path)
		if e != nil {
			return fmt.Errorf("modpack file %s: %w", f.Path, e)
		}
		m.log(fmt.Sprintf("Modpack file %d/%d: %s", i+1, len(p.Index.Files), filepath.Base(f.Path)))
		if e = download(f.Downloads[0], dest, f.Hashes["sha512"], nil); e != nil {
			return fmt.Errorf("modpack file %s: %w", f.Path, e)
		}
	}
	tmp := filepath.Join(m.root, "downloads", "modpack")
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if e := extract(p.Archive, tmp); e != nil {
		return e
	}
	for _, dir := range []string{"overrides", "server-overrides"} {
		if _, e := os.Stat(filepath.Join(tmp, dir)); e == nil {
			if e = copyTree(filepath.Join(tmp, dir), game); e != nil {
				return e
			}
		}
	}
	os.Remove(p.Archive)
	return nil
}
