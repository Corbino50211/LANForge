package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// AMP stage types LANForge can run. GitRepo still needs git and is left out.
var supportedStages = map[string]bool{"SteamCMD": true, "CreateDirectory": true, "CreateFile": true, "CopyFilePath": true, "FetchURL": true, "FetchURLFromJQ": true, "GithubRelease": true, "ExtractArchive": true, "SetExecutableFlag": true, "Executable": true, "Pause": true, "StartApplication": true, "WaitForStartupComplete": true, "ShutdownApplication": true}

func stageApplies(st map[string]any, platform string) bool {
	plat := strings.ToLower(str(st["UpdateSourcePlatform"]))
	arch := strings.ToLower(str(st["UpdateSourceArch"]))
	return (plat == "" || plat == "all" || plat == platform) && (arch == "" || arch == "x86_64")
}

// configType falls back to the file extension when a template omits ConfigType.
func configType(meta map[string]any) string {
	if t := strings.ToLower(str(meta["ConfigType"])); t != "" {
		return t
	}
	switch strings.ToLower(filepath.Ext(str(meta["ConfigFile"]))) {
	case ".ini":
		return "ini"
	case ".json":
		return "json"
	case ".xml":
		return "xml"
	}
	return "kvp"
}
func genericCompatibility(t Template, platform string) string {
	for _, v := range strings.Split(t.KVP["App.Compatibility"], ",") {
		if v = strings.TrimSpace(v); v != "" && v != "None" && !strings.HasSuffix(v, "BufferedConsole") {
			return "Requires compatibility runtime: " + v
		}
	}
	applicable := 0
	for _, st := range append(append([]map[string]any{}, t.Updates...), t.PreStart...) {
		if !stageApplies(st, platform) {
			continue
		}
		applicable++
		src := str(st["UpdateSource"])
		if !supportedStages[src] {
			return "Installer requires " + src + " stage support"
		}
		if f := str(st["ForceDownloadPlatform"]); f != "" && !strings.EqualFold(f, platform) {
			return "Requires the " + f + " build, which needs Wine on " + platform
		}
	}
	if applicable == 0 {
		return "No installation stages for " + platform
	}
	if v := t.KVP["App.UserActions"]; v != "" && v != "[]" {
		return "Requires custom startup actions"
	}
	for _, meta := range t.Meta {
		if plat := strings.ToLower(str(meta["ConfigFilePlatform"])); plat != "" && plat != "all" && plat != platform {
			continue
		}
		typ := configType(meta)
		if typ != "ini" && typ != "kvp" && typ != "json" && typ != "template" {
			return "Requires " + typ + " configuration mapping"
		}
		format := str(meta["ConfigFormat"])
		if format != "" && format != "{0}{1}" && (!strings.Contains(format, "{0}") || !strings.Contains(format, "{1}") || (typ == "ini" && !strings.Contains(format, "="))) {
			return "Requires a custom configuration format"
		}
		if asBool(meta["ConfigFileIsVariable"]) {
			return "Requires variable configuration files"
		}
	}
	key := "App.ExecutableLinux"
	if platform == "windows" {
		key = "App.ExecutableWin"
	}
	if v := strings.ReplaceAll(t.KVP[key], "\\", "/"); v == "" || strings.HasPrefix(v, "/") {
		return "Requires an external runtime on " + platform
	}
	return ""
}
func compilePlan(t Template, s Server, platform string) (*Plan, error) {
	if t.Engine != "amp" {
		return &Plan{Engine: t.Engine}, nil
	}
	if reason := genericCompatibility(t, platform); reason != "" {
		return nil, errors.New(reason)
	}
	norm := func(v string) string { return strings.TrimPrefix(strings.ReplaceAll(v, "\\", "/"), "./") }
	root := filepath.ToSlash(filepath.Join("game", norm(t.KVP["App.RootDir"])))
	base := filepath.ToSlash(filepath.Join("game", norm(t.KVP["App.BaseDirectory"])))
	if t.KVP["App.BaseDirectory"] == "" {
		base = root
	}
	vars := map[string]string{"$ApplicationIPBinding": "0.0.0.0", "$MaxUsers": t.KVP["App.MaxUsers"], "$RemoteAdminPassword": token(12), "$FullRootDir": "$ROOT/" + root + "/", "$FullBaseDir": "$ROOT/" + base + "/", "$RootDir": root + "/", "$BaseDirectory": base + "/", "$FullInstanceDir": "$ROOT/game/", "$ExternalIP": "0.0.0.0"}
	if vars["$MaxUsers"] == "" {
		vars["$MaxUsers"] = "20"
	}
	for _, f := range t.RawFields {
		if _, exists := vars[str(f["FieldName"])]; !exists {
			vars[str(f["FieldName"])] = str(f["DefaultValue"])
		}
	}
	for k, v := range s.Settings {
		if v != "" || vars[k] == "" {
			vars[k] = v
		}
	}
	for _, p := range s.Ports {
		vars["$"+p.ID] = strconv.Itoa(p.Number)
	}
	exeKey, argKey := "App.ExecutableLinux", "App.LinuxCommandLineArgs"
	if platform == "windows" {
		exeKey, argKey = "App.ExecutableWin", "App.WindowsCommandLineArgs"
	}
	vars["$PlatformArgs"] = t.KVP[argKey]
	expand := func(v string) (string, error) {
		for i := 0; i < 10 && variableRE.MatchString(v); i++ {
			missing := []string{}
			v = variableRE.ReplaceAllStringFunc(v, func(match string) string {
				k := variableRE.FindStringSubmatch(match)[1]
				if x, ok := templateFunc(k, platform); ok {
					return x
				}
				x, ok := vars[k]
				if !ok {
					// Some templates drop the leading $ on built-ins, e.g. {{FullBaseDir}}.
					x, ok = vars["$"+k]
				}
				if !ok {
					missing = append(missing, k)
				}
				return x
			})
			if len(missing) > 0 {
				return "", fmt.Errorf("unsupported AMP variables: %s", strings.Join(missing, ", "))
			}
		}
		if variableRE.MatchString(v) {
			return "", errors.New("recursive template variables")
		}
		return v, nil
	}
	quote := func(v string) string { return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"` }
	parts := []string{}
	format := t.KVP["App.CommandLineParameterFormat"]
	if format == "" {
		format = "{0}={1}"
	}
	delimiter := t.KVP["App.CommandLineParameterDelimiter"]
	if delimiter == "" {
		delimiter = " "
	}
	for _, f := range t.RawFields {
		if !asBool(f["IncludeInCommandLine"]) {
			continue
		}
		val, e := expand(vars[str(f["FieldName"])])
		if e != nil {
			return nil, e
		}
		if asBool(f["SkipIfEmpty"]) && val == "" {
			continue
		}
		if asBool(f["IsFlagArgument"]) {
			if val != "" && val != "false" && val != "0" {
				parts = append(parts, val)
			}
			continue
		}
		param := str(f["ParamFieldName"])
		if param == "" {
			param = str(f["FieldName"])
		}
		part := strings.ReplaceAll(format, "{0}", param)
		if strings.Contains(part, `"{1}"`) {
			part = strings.ReplaceAll(part, `"{1}"`, quote(val))
		} else {
			part = strings.ReplaceAll(part, "{1}", quote(val))
		}
		parts = append(parts, part)
	}
	vars["$FormattedArgs"] = strings.Join(parts, delimiter)
	executable, e := expand(t.KVP[exeKey])
	if e != nil {
		return nil, e
	}
	if executable == "" {
		return nil, errors.New("template has no executable on this platform")
	}
	args, e := expand(t.KVP["App.CommandLineArgs"])
	if e != nil {
		return nil, e
	}
	argv, e := splitArgs(args)
	if e != nil {
		return nil, e
	}
	work, e := expand(t.KVP["App.WorkingDir"])
	if e != nil {
		return nil, e
	}
	p := &Plan{Engine: "amp", Executable: filepath.ToSlash(filepath.Join(root, norm(executable))), WorkDir: filepath.ToSlash(filepath.Join(root, norm(work))), Args: argv, Env: map[string]string{}}
	if t.KVP["App.ExitMethod"] == "String" {
		p.Stop = t.KVP["App.ExitString"]
	}
	var env map[string]string
	if raw := t.KVP["App.EnvironmentVariables"]; raw != "" {
		if e = json.Unmarshal([]byte(raw), &env); e != nil {
			return nil, e
		}
		for k, v := range env {
			p.Env[k], e = expand(v)
			if e != nil {
				return nil, e
			}
		}
	}
	relPath := func(v string) (string, error) {
		v = strings.TrimPrefix(v, "$ROOT/")
		if _, e := inside("/instance", v); e != nil {
			return "", e
		}
		return norm(v), nil
	}
	// Relative stage paths are relative to the AMP instance directory, which is game/.
	stagePath := func(v string) (string, error) {
		if !strings.HasPrefix(v, "$ROOT/") {
			v = "game/" + norm(v)
		}
		return relPath(v)
	}
	compileStages := func(stages []map[string]any) ([]Step, error) {
		out := []Step{}
		for _, st := range stages {
			if !stageApplies(st, platform) {
				continue
			}
			if k := str(st["UpdateSourceConditionSetting"]); k != "" && vars[k] != str(st["UpdateSourceConditionValue"]) {
				continue
			}
			src := str(st["UpdateSource"])
			step := Step{Type: strings.ToLower(src), Name: str(st["UpdateStageName"]), Extract: asBool(st["UnzipUpdateSource"]), Overwrite: asBool(st["OverwriteExistingFiles"]), Skip: asBool(st["SkipOnFailure"]), Platform: strings.ToLower(str(st["ForceDownloadPlatform"]))}
			if step.Name == "" {
				step.Name = src
			}
			var e error
			if step.Data, e = expand(str(st["UpdateSourceData"])); e != nil {
				return nil, e
			}
			if step.Args, e = expand(str(st["UpdateSourceArgs"])); e != nil {
				return nil, e
			}
			target, e := expand(str(st["UpdateSourceTarget"]))
			if e != nil {
				return nil, e
			}
			switch src {
			case "SteamCMD":
				if _, e = strconv.Atoi(step.Data); e != nil {
					return nil, errors.New("invalid Steam application ID")
				}
				step.Type, step.AppID, step.Args = "steam", step.Data, ""
				step.Login = !asBool(t.KVP["App.SteamUpdateAnonymousLogin"])
				if target == "" {
					target = "$ROOT/" + root + "/" + step.AppID
				}
				if step.Branch, e = expand(str(st["UpdateSourceVersion"])); e != nil {
					return nil, e
				}
			case "GithubRelease":
				if step.Branch, e = expand(str(st["UpdateSourceVersion"])); e != nil {
					return nil, e
				}
			case "CreateDirectory", "CreateFile", "SetExecutableFlag":
				if step.Args, e = stagePath(step.Args); e != nil {
					return nil, e
				}
			case "CopyFilePath":
				if step.Data, e = stagePath(step.Data); e != nil {
					return nil, e
				}
				if step.Args, e = stagePath(step.Args); e != nil {
					return nil, e
				}
			case "ExtractArchive":
				if step.Data, e = stagePath(step.Data); e != nil {
					return nil, e
				}
			}
			if step.Target, e = stagePath(target); e != nil {
				return nil, e
			}
			out = append(out, step)
		}
		return out, nil
	}
	if p.Steps, e = compileStages(t.Updates); e != nil {
		return nil, e
	}
	if p.PreStart, e = compileStages(t.PreStart); e != nil {
		return nil, e
	}
	for _, meta := range t.Meta {
		plat := strings.ToLower(str(meta["ConfigFilePlatform"]))
		if plat != "" && plat != "all" && plat != platform {
			continue
		}
		path, e := expand(str(meta["ConfigFile"]))
		if e != nil {
			return nil, e
		}
		if !strings.HasPrefix(path, "$ROOT/") {
			path = filepath.ToSlash(filepath.Join(base, norm(path)))
		}
		path, e = relPath(path)
		if e != nil {
			return nil, e
		}
		typ := configType(meta)
		cfg := ConfigSpec{Path: path, Format: typ, Pattern: str(meta["ConfigFormat"]), Values: map[string]string{}}
		if typ == "template" {
			tpl := str(meta["ConfigFileTemplate"])
			if tpl == "" {
				return nil, errors.New("template configuration has no template file")
			}
			if cfg.Template, e = stagePath(strings.TrimPrefix(base, "game/") + "/" + norm(tpl)); e != nil {
				return nil, e
			}
			// Templates reference fields by name, e.g. {{lobby_name}}.
			for _, f := range t.RawFields {
				if k := str(f["FieldName"]); k != "" {
					if cfg.Values[k], e = expand(vars[k]); e != nil {
						return nil, e
					}
				}
			}
		}
		put := func(key, field string, section string) error {
			if key == "" && (typ == "ini" || cfg.Pattern != "{0}{1}") {
				return nil // free-form extra lines; the defaults are empty
			}
			if cfg.Pattern == "{0}{1}" && strings.HasSuffix(key, "=") {
				key, cfg.Pattern = strings.TrimSuffix(key, "="), ""
			}
			val, ok := vars[field]
			if !ok {
				// AMP mappings may hold a constant instead of a field name, e.g. "True".
				val = field
			}
			val, e = expand(val)
			if e != nil {
				return e
			}
			if section != "" {
				key = section + "\x1f" + key
			} else if typ == "ini" {
				if i := strings.LastIndex(key, "."); i > 0 {
					key = key[:i] + "\x1f" + key[i+1:]
				}
			}
			if key == "" {
				// The whole file is the value, e.g. a token file.
				cfg.Format, cfg.Values = "raw", map[string]string{"": val}
				return nil
			}
			cfg.Values[key] = val
			return nil
		}
		if asBool(meta["AutoMap"]) {
			for _, f := range t.RawFields {
				if asBool(f["IncludeInCommandLine"]) || strings.HasPrefix(str(f["Special"]), "listfile:") {
					continue
				}
				key := str(f["ParamFieldName"])
				if key == "" {
					continue
				}
				if e = put(key, str(f["FieldName"]), ""); e != nil {
					return nil, e
				}
			}
		}
		mappings := func(m any, section string) error {
			if mm, ok := m.(map[string]any); ok {
				for k, v := range mm {
					if e := put(k, str(v), section); e != nil {
						return e
					}
				}
			}
			return nil
		}
		if e = mappings(meta["SettingMappings"], ""); e != nil {
			return nil, e
		}
		if subs, ok := meta["Subsections"].([]any); ok {
			for _, sub := range subs {
				if sm, ok := sub.(map[string]any); ok {
					if e = mappings(sm["SettingMappings"], str(sm["Heading"])); e != nil {
						return nil, e
					}
				}
			}
		}
		p.Configs = append(p.Configs, cfg)
	}
	for _, v := range []string{p.Executable, p.WorkDir} {
		if _, e = relPath(v); e != nil {
			return nil, e
		}
	}
	return p, nil
}

var envVariableRE = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_]*)%`)

func applyAMPConfigs(m *Managed) error {
	s := m.specCopy()
	for _, cfg := range s.Plan.Configs {
		path, e := inside(m.root, cfg.Path)
		if e != nil {
			return e
		}
		values := map[string]string{}
		for k, v := range cfg.Values {
			values[k] = strings.ReplaceAll(v, "$ROOT", m.root)
		}
		switch {
		case cfg.Format == "template":
			e = fillTemplate(m, cfg, path, values)
		case cfg.Format == "kvp" && cfg.Pattern != "" && cfg.Pattern != "{0}={1}":
			e = mergeFormatted(path, cfg.Pattern, values)
		case cfg.Format == "kvp":
			e = mergeProperties(path, values)
		case cfg.Format == "ini":
			e = mergeINI(path, values, cfg.Pattern)
		case cfg.Format == "json":
			e = mergeJSON(path, values)
		case cfg.Format == "raw":
			os.MkdirAll(filepath.Dir(path), 0700)
			e = atomicWrite(path, []byte(values[""]), 0600)
		default:
			e = errors.New("unsupported config format")
		}
		if e != nil {
			return e
		}
	}
	return nil
}
func mergeINI(path string, updates map[string]string, format string) error {
	if format == "" {
		format = "{0}={1}"
	}
	formatLine := func(k, v string) string { return strings.NewReplacer("{0}", k, "{1}", v).Replace(format) }
	sections := map[string][]string{}
	order := []string{""}
	section := ""
	seen := map[string]bool{}
	if f, e := os.Open(path); e == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 4096), 2<<20)
		for sc.Scan() {
			line := sc.Text()
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
				section = strings.TrimSpace(trim[1 : len(trim)-1])
				if _, ok := sections[section]; !ok {
					order = append(order, section)
					sections[section] = []string{}
				}
				continue
			}
			key, _, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			id := key
			if section != "" {
				id = section + "\x1f" + key
			}
			if v, exists := updates[id]; ok && exists && !strings.HasPrefix(key, ";") && !strings.HasPrefix(key, "#") {
				line = formatLine(key, v)
				seen[id] = true
			}
			sections[section] = append(sections[section], line)
		}
		e = sc.Err()
		f.Close()
		if e != nil {
			return e
		}
	}
	keys := []string{}
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, id := range keys {
		if seen[id] {
			continue
		}
		sec, key := "", id
		if a, b, ok := strings.Cut(id, "\x1f"); ok {
			sec, key = a, b
		}
		if _, ok := sections[sec]; !ok {
			sections[sec] = []string{}
			if sec != "" {
				order = append(order, sec)
			}
		}
		sections[sec] = append(sections[sec], formatLine(key, updates[id]))
	}
	var out strings.Builder
	for _, sec := range order {
		if sec != "" {
			out.WriteString("\n[" + sec + "]\n")
		}
		for _, line := range sections[sec] {
			out.WriteString(line + "\n")
		}
	}
	return atomicWrite(path, []byte(out.String()), 0600)
}
func mergeJSON(path string, updates map[string]string) error {
	root := map[string]any{}
	if b, e := os.ReadFile(path); e == nil {
		if e = json.Unmarshal(b, &root); e != nil {
			return fmt.Errorf("preserving malformed JSON %s: %w", filepath.Base(path), e)
		}
	}
	for k, v := range updates {
		parts := strings.Split(k, ".")
		target := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := target[part].(map[string]any)
			if !ok {
				if _, exists := target[part]; exists {
					return fmt.Errorf("config path %s crosses a non-object", k)
				}
				next = map[string]any{}
				target[part] = next
			}
			target = next
		}
		leaf := parts[len(parts)-1]
		var val any = v
		if old, exists := target[leaf]; exists {
			switch old.(type) {
			case bool:
				if x, e := strconv.ParseBool(v); e == nil {
					val = x
				}
			case float64:
				if x, e := strconv.ParseFloat(v, 64); e == nil {
					val = x
				}
			}
		}
		target[leaf] = val
	}
	return writeJSON(path, root)
}

// templateFunc evaluates AMP's built-in template expressions.
func templateFunc(k, platform string) (string, bool) {
	if k == "newguid()" {
		b := []byte(token(16))[:32]
		return fmt.Sprintf("%s-%s-%s-%s-%s", b[:8], b[8:12], b[12:16], b[16:20], b[20:]), true
	}
	var n int
	if _, e := fmt.Sscanf(k, "randomdigits(%d)", &n); e == nil && n > 0 && n <= 64 {
		out := []byte(token(n))
		for i := range out {
			out[i] = '0' + out[i]%10
		}
		return string(out[:n]), true
	}
	// "Windows:a, Linux:b" picks the value for this platform.
	if strings.Contains(k, "Windows:") || strings.Contains(k, "Linux:") {
		for _, part := range strings.Split(k, ",") {
			if p, v, ok := strings.Cut(strings.TrimSpace(part), ":"); ok && strings.EqualFold(p, platform) {
				return v, true
			}
		}
		return "", true
	}
	return "", false
}

// mergeFormatted updates "key value" style files using AMP's ConfigFormat, e.g. `set {0} "{1}"`.
func mergeFormatted(path, format string, updates map[string]string) error {
	formatLine := func(k, v string) string { return strings.NewReplacer("{0}", k, "{1}", v).Replace(format) }
	word := func(c byte) bool {
		return c == '_' || c == '-' || c == '.' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
	}
	lines := []string{}
	if b, e := os.ReadFile(path); e == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	}
	keys := []string{}
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	for i, l := range lines {
		t := strings.TrimLeft(strings.TrimRight(l, "\r"), " \t")
		for _, k := range keys {
			f := formatLine(k, "\x00")
			p := strings.TrimRight(strings.TrimLeft(f[:strings.Index(f, "\x00")], " \t"), ` "'`)
			if p != "" && strings.HasPrefix(t, p) && (len(t) == len(p) || !word(t[len(p)]) || !word(p[len(p)-1])) {
				lines[i] = formatLine(k, updates[k])
				seen[k] = true
				break
			}
		}
	}
	for _, k := range keys {
		if !seen[k] {
			lines = append(lines, formatLine(k, updates[k]))
		}
	}
	return atomicWrite(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
}

// fillTemplate writes path from a template file with {{Field}} placeholders.
func fillTemplate(m *Managed, cfg ConfigSpec, path string, values map[string]string) error {
	src, e := inside(m.root, cfg.Template)
	if e != nil {
		return e
	}
	b, e := os.ReadFile(src)
	if e != nil {
		return fmt.Errorf("configuration template missing: %w", e)
	}
	out := variableRE.ReplaceAllStringFunc(string(b), func(match string) string {
		return values[variableRE.FindStringSubmatch(match)[1]]
	})
	os.MkdirAll(filepath.Dir(path), 0700)
	return atomicWrite(path, []byte(out), 0600)
}
