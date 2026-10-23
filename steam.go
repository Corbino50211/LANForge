package main

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
)

var digitsRE = regexp.MustCompile(`^[0-9]+$`)

// steamApp returns the Steam store app a template's game belongs to, e.g. 4000 for Garry's Mod.
func steamApp(kv map[string]string, updates []map[string]any) string {
	var env map[string]any
	if json.Unmarshal([]byte(kv["App.EnvironmentVariables"]), &env) == nil && digitsRE.MatchString(str(env["SteamAppId"])) {
		return str(env["SteamAppId"])
	}
	for _, u := range updates {
		if a := str(u["UpdateSourceArgs"]); str(u["UpdateSource"]) == "SteamCMD" && digitsRE.MatchString(a) {
			return a
		}
	}
	return ""
}

var steamIconMu sync.Mutex

// steamIcons maps app IDs to their Steam community icon, caching lookups (including misses) in the data folder.
func steamIcons(data string, ids []string) map[string]string {
	steamIconMu.Lock()
	defer steamIconMu.Unlock()
	path := filepath.Join(data, "steam-icons.json")
	icons := map[string]string{}
	readJSON(path, &icons)
	missing := []map[string]int{}
	for _, id := range ids {
		if _, ok := icons[id]; !ok {
			n, _ := strconv.Atoi(id)
			missing = append(missing, map[string]int{"appid": n})
		}
	}
	changed := false
	for len(missing) > 0 {
		chunk := missing[:min(100, len(missing))]
		missing = missing[len(chunk):]
		q, _ := json.Marshal(map[string]any{"ids": chunk, "context": map[string]string{"country_code": "US"}, "data_request": map[string]bool{"include_assets": true}})
		var res struct {
			Response struct {
				Items []struct {
					AppID  int `json:"appid"`
					Assets struct {
						Icon string `json:"community_icon"`
					} `json:"assets"`
				} `json:"store_items"`
			} `json:"response"`
		}
		if fetchJSON("https://api.steampowered.com/IStoreBrowseService/GetItems/v1?input_json="+url.QueryEscape(string(q)), &res) != nil {
			break
		}
		for _, c := range chunk {
			icons[strconv.Itoa(c["appid"])] = ""
		}
		for _, it := range res.Response.Items {
			if it.Assets.Icon != "" {
				icons[strconv.Itoa(it.AppID)] = "https://cdn.cloudflare.steamstatic.com/steamcommunity/public/images/apps/" + strconv.Itoa(it.AppID) + "/" + it.Assets.Icon + ".jpg"
			}
		}
		changed = true
	}
	if changed {
		writeJSON(path, icons)
	}
	return icons
}
