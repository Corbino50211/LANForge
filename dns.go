package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var zoneRE = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)
var hostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

func cloudflare(key, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, "https://api.cloudflare.com/client/v4"+path, reader)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 25 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&env); e != nil {
		return nil, fmt.Errorf("Cloudflare returned HTTP %d", res.StatusCode)
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return nil, errors.New("Cloudflare: " + env.Errors[0].Message)
		}
		return nil, fmt.Errorf("Cloudflare returned HTTP %d", res.StatusCode)
	}
	return env.Result, nil
}
func (c *Controller) cloudflareSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
		Zone  string `json:"zone"`
	}
	if e := decode(w, r, &in); e != nil {
		apiError(w, e)
		return
	}
	if !zoneRE.MatchString(in.Zone) || in.Token == "" {
		apiError(w, errors.New("enter a Cloudflare zone ID and a token with Zone Read and DNS Edit for that zone"))
		return
	}
	if _, e := cloudflare(in.Token, "GET", "/zones/"+in.Zone, nil); e != nil {
		apiError(w, e)
		return
	}
	if e := c.store.secret("cfToken", in.Token); e != nil {
		apiError(w, e)
		return
	}
	if e := c.store.put("settings", "cfZone", in.Zone); e != nil {
		apiError(w, e)
		return
	}
	jsonReply(w, 200, map[string]bool{"ok": true})
}
func publicIP() (string, error) {
	var out struct {
		IP string `json:"ip"`
	}
	if e := fetchJSON("https://api.ipify.org?format=json", &out); e != nil {
		return "", e
	}
	ip := net.ParseIP(out.IP)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", errors.New("could not discover a public IPv4 address")
	}
	return ip.String(), nil
}
func (c *Controller) configureDNS(s *Server, args map[string]string) error {
	c.dnsMu.Lock()
	defer c.dnsMu.Unlock()
	c.createMu.Lock()
	defer c.createMu.Unlock()
	if err := c.store.get("servers", s.ID, s); err != nil {
		return err
	}
	if s.Mode != "direct" {
		return errors.New("automatic DNS is currently for port-forwarded servers")
	}
	var zone string
	c.store.get("settings", "cfZone", &zone)
	key, e := c.store.unsecret("cfToken")
	if e != nil || key == "" || !zoneRE.MatchString(zone) {
		return errors.New("connect Cloudflare in Settings first")
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(args["hostname"]), "."))
	if !hostRE.MatchString(host) || strings.Contains(host, "..") {
		return errors.New("enter a valid hostname, such as minecraft.example.com")
	}
	data, e := cloudflare(key, "GET", "/zones/"+zone, nil)
	if e != nil {
		return e
	}
	var z struct {
		Name string `json:"name"`
	}
	json.Unmarshal(data, &z)
	if !strings.HasSuffix(host, "."+z.Name) {
		return errors.New("choose a subdomain inside the connected zone; the root domain is protected")
	}
	ip := strings.TrimSpace(args["publicIP"])
	fixed := ip != ""
	if ip == "" {
		ip, e = publicIP()
		if e != nil {
			return e
		}
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil || parsed.IsPrivate() || !parsed.IsGlobalUnicast() {
		return errors.New("enter a public IPv4 address")
	}
	port := s.Ports[0].Number
	if args["port"] != "" {
		port, e = strconv.Atoi(args["port"])
		if e != nil || port < 1 || port > 65535 {
			return errors.New("invalid public port")
		}
	}
	base := "/zones/" + zone + "/dns_records"
	data, e = cloudflare(key, "GET", base+"?name="+url.QueryEscape(host), nil)
	if e != nil {
		return e
	}
	var records []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	json.Unmarshal(data, &records)
	id := ""
	for _, rec := range records {
		if s.DNS == nil || rec.ID != s.DNS.RecordID {
			return errors.New("that hostname already has DNS records. Choose an unused subdomain to avoid replacing existing records")
		}
		if rec.Type == "A" {
			id = rec.ID
		}
	}
	method := "POST"
	path := base
	if id != "" {
		method = "PUT"
		path += "/" + id
	}
	body := map[string]any{"type": "A", "name": host, "content": ip, "ttl": 120, "proxied": false, "comment": "Managed by LANForge server " + s.ID}
	data, e = cloudflare(key, method, path, body)
	if e != nil {
		return e
	}
	var rec struct {
		ID string `json:"id"`
	}
	json.Unmarshal(data, &rec)
	srvID := ""
	if s.DNS != nil && s.DNS.Hostname == host {
		srvID = s.DNS.SRVID
	}
	s.DNS = &DNSRecord{Hostname: host, RecordID: rec.ID, Port: port, LastIP: ip, Updated: time.Now(), StaticIP: fixed, SRVID: srvID}
	if e = c.store.put("servers", s.ID, *s); e != nil {
		return e
	}
	if args["srv"] == "true" && s.Engine == "minecraft" {
		name := "_minecraft._tcp." + host
		data, e = cloudflare(key, "GET", base+"?type=SRV&name="+url.QueryEscape(name), nil)
		if e != nil {
			return e
		}
		records = nil
		json.Unmarshal(data, &records)
		for _, r := range records {
			if r.ID != srvID {
				return errors.New("an SRV record already exists; A record saved, existing SRV preserved")
			}
		}
		method = "POST"
		path = base
		if srvID != "" {
			method = "PUT"
			path += "/" + srvID
		}
		data, e = cloudflare(key, method, path, map[string]any{"type": "SRV", "name": name, "ttl": 120, "data": map[string]any{"priority": 0, "weight": 5, "port": port, "target": host}, "comment": "Managed by LANForge server " + s.ID})
		if e != nil {
			s.DNS.Error = "A record saved; SRV failed: " + e.Error()
			c.store.put("servers", s.ID, *s)
			return errors.New(s.DNS.Error)
		}
		json.Unmarshal(data, &rec)
		s.DNS.SRVID = rec.ID
	}
	return c.store.put("servers", s.ID, *s)
}
func (c *Controller) dnsLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.syncDNS()
		}
	}
}
func (c *Controller) syncDNS() {
	c.dnsMu.Lock()
	defer c.dnsMu.Unlock()
	c.createMu.Lock()
	defer c.createMu.Unlock()
	var list []Server
	if c.store.list("servers", &list) != nil {
		return
	}
	needed := false
	for _, s := range list {
		if s.DNS != nil && !s.DNS.StaticIP {
			needed = true
		}
	}
	if !needed {
		return
	}
	key, e := c.store.unsecret("cfToken")
	if e != nil {
		return
	}
	var zone string
	c.store.get("settings", "cfZone", &zone)
	ip, e := publicIP()
	if e != nil {
		return
	}
	for _, s := range list {
		if s.DNS == nil || s.DNS.StaticIP || s.DNS.LastIP == ip {
			continue
		}
		_, e := cloudflare(key, "PATCH", "/zones/"+zone+"/dns_records/"+s.DNS.RecordID, map[string]any{"content": ip, "proxied": false})
		if e != nil {
			s.DNS.Error = e.Error()
		} else {
			s.DNS.LastIP = ip
			s.DNS.Updated = time.Now()
			s.DNS.Error = ""
		}
		c.store.put("servers", s.ID, s)
	}
}
