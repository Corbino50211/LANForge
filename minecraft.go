package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// installMinecraft installs vanilla or a mod/plugin loader, plus an optional Modrinth modpack.
func installMinecraft(m *Managed) error {
	s := m.specCopy()
	loader := strings.ToLower(s.Settings["loader"])
	if loader == "" {
		loader = "vanilla"
	}
	wanted, loaderVersion := s.Settings["version"], s.Settings["loaderVersion"]
	if loaderVersion == "latest" {
		loaderVersion = ""
	}
	var pack *modpack
	if ref := strings.TrimSpace(s.Settings["modpack"]); ref != "" {
		p, e := fetchModpack(m, ref, wanted)
		if e != nil {
			return e
		}
		pack = p
		// The pack decides the game version and loader.
		wanted = p.Index.Dependencies["minecraft"]
		for dep, l := range map[string]string{"fabric-loader": "fabric", "quilt-loader": "quilt", "forge": "forge", "neoforge": "neoforge"} {
			if v := p.Index.Dependencies[dep]; v != "" {
				loader, loaderVersion = l, v
			}
		}
		m.log(fmt.Sprintf("Modpack %s needs Minecraft %s with %s", p.Name, wanted, loader))
	}
	m.log("Finding Minecraft " + wanted)
	var manifest struct {
		Latest struct {
			Release string `json:"release"`
		} `json:"latest"`
		Versions []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"versions"`
	}
	if e := fetchJSON("https://piston-meta.mojang.com/mc/game/version_manifest_v2.json", &manifest); e != nil {
		return e
	}
	if wanted == "" || wanted == "latest" {
		wanted = manifest.Latest.Release
	}
	metadataURL := ""
	for _, v := range manifest.Versions {
		if v.ID == wanted {
			metadataURL = v.URL
		}
	}
	if metadataURL == "" {
		return errors.New("Minecraft version not found; use latest or an official release number")
	}
	var meta struct {
		Downloads struct {
			Server struct {
				URL  string `json:"url"`
				SHA1 string `json:"sha1"`
			} `json:"server"`
		} `json:"downloads"`
		JavaVersion struct {
			Major int `json:"majorVersion"`
		} `json:"javaVersion"`
	}
	if e := fetchJSON(metadataURL, &meta); e != nil {
		return e
	}
	major := meta.JavaVersion.Major
	if major == 0 {
		major = 17
	}
	java, e := installJava(m, major)
	if e != nil {
		return e
	}
	game := filepath.Join(m.root, "game")
	jar := filepath.Join(game, "server.jar")
	args := []string{"-jar", "server.jar", "nogui"}
	switch loader {
	case "vanilla":
		if meta.Downloads.Server.URL == "" {
			return errors.New("this Minecraft version has no dedicated server download")
		}
		e = download(meta.Downloads.Server.URL, jar, meta.Downloads.Server.SHA1, m.log)
	case "paper":
		e = installPaper(m, wanted, loaderVersion, jar)
	case "purpur":
		e = installPurpur(m, wanted, loaderVersion, jar)
	case "fabric":
		e = installFabric(m, wanted, loaderVersion, jar)
	case "quilt":
		args = []string{"-jar", "quilt-server-launch.jar", "nogui"}
		e = installQuilt(m, java, wanted, loaderVersion)
	case "forge":
		args, e = installForge(m, java, wanted, loaderVersion)
	case "neoforge":
		args, e = installNeoForge(m, java, wanted, loaderVersion)
	default:
		e = errors.New("unknown Minecraft loader " + loader)
	}
	if e != nil {
		return e
	}
	if pack != nil {
		if e = applyModpack(m, pack); e != nil {
			return e
		}
	}
	launch, _ := json.Marshal(args)
	m.mu.Lock()
	m.spec.Settings["installedVersion"] = wanted
	m.spec.Settings["installedLoader"] = loader
	m.spec.Settings["launchArgs"] = string(launch)
	m.spec.Plan = &Plan{Engine: "minecraft", Executable: java, WorkDir: "game", Stop: "stop"}
	m.mu.Unlock()
	// Mods picked in the create wizard. One that doesn't fit this version shouldn't fail the whole install.
	for _, id := range strings.FieldsFunc(s.Settings["mods"], func(r rune) bool { return r == ',' || r == ' ' }) {
		if _, e := installMods(m, id); e != nil {
			m.log("Skipped a mod: " + e.Error())
		}
	}
	return nil
}

// minecraftArgs are the java arguments after the memory flags; older installs predate launchArgs.
func minecraftArgs(s Server) []string {
	var args []string
	if json.Unmarshal([]byte(s.Settings["launchArgs"]), &args) != nil || len(args) == 0 {
		args = []string{"-jar", "server.jar", "nogui"}
	}
	return args
}

func installPaper(m *Managed, mc, build, jar string) error {
	if build == "" {
		build = "latest"
	}
	var b struct {
		Downloads map[string]struct {
			URL       string            `json:"url"`
			Checksums map[string]string `json:"checksums"`
		} `json:"downloads"`
	}
	if e := fetchJSON("https://fill.papermc.io/v3/projects/paper/versions/"+mc+"/builds/"+build, &b); e != nil {
		return fmt.Errorf("Paper has no build for Minecraft %s yet (%w)", mc, e)
	}
	d := b.Downloads["server:default"]
	m.log("Downloading Paper for Minecraft " + mc)
	return download(d.URL, jar, d.Checksums["sha256"], m.log)
}
func installPurpur(m *Managed, mc, build, jar string) error {
	if build == "" {
		build = "latest"
	}
	base := "https://api.purpurmc.org/v2/purpur/" + mc + "/" + build
	var b struct {
		MD5 string `json:"md5"`
	}
	if e := fetchJSON(base, &b); e != nil {
		return fmt.Errorf("Purpur has no build for Minecraft %s yet (%w)", mc, e)
	}
	m.log("Downloading Purpur for Minecraft " + mc)
	return download(base+"/download", jar, b.MD5, m.log)
}
func installFabric(m *Managed, mc, loader, jar string) error {
	if loader == "" {
		var loaders []struct {
			Loader struct {
				Version string `json:"version"`
				Stable  bool   `json:"stable"`
			} `json:"loader"`
		}
		if e := fetchJSON("https://meta.fabricmc.net/v2/versions/loader/"+mc, &loaders); e != nil || len(loaders) == 0 {
			return fmt.Errorf("Fabric does not support Minecraft %s yet", mc)
		}
		loader = loaders[0].Loader.Version
		for _, l := range loaders {
			if l.Loader.Stable {
				loader = l.Loader.Version
				break
			}
		}
	}
	var installers []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if e := fetchJSON("https://meta.fabricmc.net/v2/versions/installer", &installers); e != nil || len(installers) == 0 {
		return errors.New("could not read Fabric installer versions")
	}
	installer := installers[0].Version
	for _, i := range installers {
		if i.Stable {
			installer = i.Version
			break
		}
	}
	m.log("Downloading Fabric " + loader + " for Minecraft " + mc)
	return download(fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s/%s/server/jar", mc, loader, installer), jar, "", m.log)
}
func installQuilt(m *Managed, java, mc, loader string) error {
	var meta struct {
		Release string `xml:"versioning>release"`
	}
	if e := fetchXML("https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/maven-metadata.xml", &meta); e != nil || meta.Release == "" {
		return errors.New("could not read Quilt installer versions")
	}
	v := meta.Release
	installer := filepath.Join(m.root, "downloads", "quilt-installer.jar")
	if e := download(fmt.Sprintf("https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/%s/quilt-installer-%s.jar", v, v), installer, "", m.log); e != nil {
		return e
	}
	args := []string{"-jar", installer, "install", "server", mc}
	if loader != "" {
		args = append(args, loader)
	}
	game := filepath.Join(m.root, "game")
	m.log("Running the Quilt installer")
	return runLogged(m, game, nil, filepath.Join(m.root, java), append(args, "--download-server", "--install-dir="+game)...)
}
func installForge(m *Managed, java, mc, v string) ([]string, error) {
	if v == "" {
		var promos struct {
			Promos map[string]string `json:"promos"`
		}
		if e := fetchJSON("https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json", &promos); e != nil {
			return nil, e
		}
		if v = promos.Promos[mc+"-recommended"]; v == "" {
			v = promos.Promos[mc+"-latest"]
		}
		if v == "" {
			return nil, fmt.Errorf("Forge does not support Minecraft %s yet", mc)
		}
	}
	full := mc + "-" + v
	if e := runInstaller(m, java, fmt.Sprintf("https://maven.minecraftforge.net/net/minecraftforge/forge/%s/forge-%s-installer.jar", full, full), "Forge "+full); e != nil {
		return nil, e
	}
	if a := argsFileLaunch(m, "libraries/net/minecraftforge/forge/"+full); a != nil {
		return a, nil
	}
	// Forge before 1.17 installs a runnable jar instead of an args file.
	jars, _ := filepath.Glob(filepath.Join(m.root, "game", "forge-"+full+"*.jar"))
	for _, j := range jars {
		if !strings.Contains(j, "installer") {
			return []string{"-jar", filepath.Base(j), "nogui"}, nil
		}
	}
	return nil, errors.New("the Forge installer did not produce a server; check the console")
}
func installNeoForge(m *Managed, java, mc, v string) ([]string, error) {
	if v == "" {
		var list struct {
			Versions []string `json:"versions"`
		}
		if e := fetchJSON("https://maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge", &list); e != nil {
			return nil, e
		}
		// The list is oldest first; take the newest stable build, else the newest beta.
		beta := ""
		for _, x := range list.Versions {
			if !strings.HasPrefix(x, neoForgePrefix(mc)) {
				continue
			}
			if strings.Contains(x, "beta") {
				beta = x
			} else {
				v = x
			}
		}
		if v == "" {
			v = beta
		}
		if v == "" {
			return nil, fmt.Errorf("NeoForge does not support Minecraft %s (it starts at 1.20.2)", mc)
		}
	}
	if e := runInstaller(m, java, fmt.Sprintf("https://maven.neoforged.net/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar", v, v), "NeoForge "+v); e != nil {
		return nil, e
	}
	if a := argsFileLaunch(m, "libraries/net/neoforged/neoforge/"+v); a != nil {
		return a, nil
	}
	return nil, errors.New("the NeoForge installer did not produce a server; check the console")
}

// neoForgePrefix maps Minecraft 1.21.4 to NeoForge 21.4., 1.21 to 21.0., and year versions like 26.1 to 26.1.
func neoForgePrefix(mc string) string {
	if !strings.HasPrefix(mc, "1.") {
		return mc + "."
	}
	v := strings.TrimPrefix(mc, "1.")
	if !strings.Contains(v, ".") {
		v += ".0"
	}
	return v + "."
}

// argsFileLaunch returns the modern Forge/NeoForge launch arguments when the installer wrote an args file.
func argsFileLaunch(m *Managed, dir string) []string {
	name := "unix_args.txt"
	if runtime.GOOS == "windows" {
		name = "win_args.txt"
	}
	if _, e := os.Stat(filepath.Join(m.root, "game", dir, name)); e != nil {
		return nil
	}
	args := []string{}
	if _, e := os.Stat(filepath.Join(m.root, "game", "user_jvm_args.txt")); e == nil {
		args = append(args, "@user_jvm_args.txt")
	}
	return append(args, "@"+dir+"/"+name, "nogui")
}
func runInstaller(m *Managed, java, url, name string) error {
	installer := filepath.Join(m.root, "downloads", "installer.jar")
	defer os.Remove(installer)
	if e := download(url, installer, "", m.log); e != nil {
		return fmt.Errorf("download %s installer: %w", name, e)
	}
	m.log("Running the " + name + " installer. This can take a few minutes.")
	if e := runLogged(m, filepath.Join(m.root, "game"), nil, filepath.Join(m.root, java), "-jar", installer, "--installServer"); e != nil {
		return fmt.Errorf("%s installer failed: %w. Check the console", name, e)
	}
	return nil
}

// runLogged runs an installer or build tool, appending its output to the server console.
func runLogged(m *Managed, dir string, env []string, exe string, args ...string) error {
	os.MkdirAll(dir, 0700)
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	prepareProcess(cmd)
	f, e := os.OpenFile(filepath.Join(m.root, "console.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	return cmd.Run()
}
func fetchXML(raw string, out any) error {
	b, e := fetchBytes(raw)
	if e != nil {
		return e
	}
	return xml.Unmarshal(b, out)
}
func fetchBytes(raw string) ([]byte, error) {
	dir, e := os.MkdirTemp("", "lanforge-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "body")
	if e = download(raw, p, "", nil); e != nil {
		return nil, e
	}
	return os.ReadFile(p)
}
