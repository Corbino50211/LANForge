package main

import "time"

const Version = "0.2.7"

type Config struct {
	Role        string `json:"role"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Controller  string `json:"controller,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	AgentID     string `json:"agentId,omitempty"`
	Token       string `json:"token,omitempty"`
}
type Port struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Number   int    `json:"number"`
	Protocol string `json:"protocol"`
	Public   bool   `json:"public"`
}
type Field struct {
	Hidden      bool              `json:"hidden,omitempty"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Default     string            `json:"default"`
	Type        string            `json:"type"`
	Options     map[string]string `json:"options,omitempty"`
}
type Template struct {
	RawFields   []map[string]any  `json:"-"`
	Meta        []map[string]any  `json:"-"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	OS          []string          `json:"os"`
	Engine      string            `json:"engine"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	Source      string            `json:"source"`
	Revision    string            `json:"revision"`
	Ports       []Port            `json:"ports"`
	Fields      []Field           `json:"fields"`
	KVP         map[string]string `json:"-"`
	Updates     []map[string]any  `json:"-"`
	PreStart    []map[string]any  `json:"-"`
	Files       map[string]string `json:"-"`
}
type Server struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	NodeID         string            `json:"nodeId"`
	TemplateID     string            `json:"templateId"`
	Game           string            `json:"game"`
	Revision       string            `json:"revision"`
	Engine         string            `json:"engine"`
	Settings       map[string]string `json:"settings"`
	Ports          []Port            `json:"ports"`
	Mode           string            `json:"mode"`
	AutoStart      bool              `json:"autoStart"`
	RestartOnCrash bool              `json:"restartOnCrash"`
	Created        time.Time         `json:"created"`
	Plan           *Plan             `json:"plan,omitempty"`
	DNS            *DNSRecord        `json:"dns,omitempty"`
}
type Plan struct {
	Configs    []ConfigSpec      `json:"configs,omitempty"`
	Engine     string            `json:"engine"`
	Executable string            `json:"executable,omitempty"`
	Args       []string          `json:"args,omitempty"`
	WorkDir    string            `json:"workDir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Stop       string            `json:"stop,omitempty"`
	Ready      string            `json:"ready,omitempty"`
	Steps      []Step            `json:"steps,omitempty"`
	PreStart   []Step            `json:"preStart,omitempty"`
	Files      map[string]string `json:"files,omitempty"`
}

type ConfigSpec struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	// Pattern is AMP's ConfigFormat line template; Template is the source file for "template" configs.
	Pattern  string            `json:"pattern,omitempty"`
	Template string            `json:"template,omitempty"`
	Values   map[string]string `json:"values"`
}
type Step struct {
	Type    string `json:"type"`
	URL     string `json:"url,omitempty"`
	Target  string `json:"target,omitempty"`
	AppID   string `json:"appId,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Extract bool   `json:"extract,omitempty"`
	// Generic AMP stage fields. Paths are relative to the server root.
	Name      string `json:"name,omitempty"`
	Data      string `json:"data,omitempty"`
	Args      string `json:"args,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	Skip      bool   `json:"skip,omitempty"`
	Login     bool   `json:"login,omitempty"`
}
type Stats struct {
	CPU       float64 `json:"cpu"`
	RAMTotal  uint64  `json:"ramTotal"`
	RAMUsed   uint64  `json:"ramUsed"`
	DiskFree  uint64  `json:"diskFree"`
	DiskTotal uint64  `json:"diskTotal"`
}
type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Address   string    `json:"address"`
	Online    bool      `json:"online"`
	LastSeen  time.Time `json:"lastSeen"`
	Stats     Stats     `json:"stats"`
	SteamUser string    `json:"steamUser,omitempty"`
	Version   string    `json:"version,omitempty"`
}
type Runtime struct {
	ID      string    `json:"id"`
	State   string    `json:"state"`
	Message string    `json:"message"`
	PID     int       `json:"pid,omitempty"`
	Started time.Time `json:"started,omitempty"`
	Logs    []string  `json:"logs,omitempty"`
	CPU     float64   `json:"cpu"`
	RAM     uint64    `json:"ram"`
	Address string    `json:"address,omitempty"`
}
type Command struct {
	ID       string            `json:"id"`
	Action   string            `json:"action"`
	Server   *Server           `json:"server,omitempty"`
	ServerID string            `json:"serverId,omitempty"`
	Args     map[string]string `json:"args,omitempty"`
}
type Wire struct {
	Type    string       `json:"type"`
	ID      string       `json:"id,omitempty"`
	Command *Command     `json:"command,omitempty"`
	Node    *Node        `json:"node,omitempty"`
	Servers []Runtime    `json:"servers,omitempty"`
	Playit  *PlayitState `json:"playit,omitempty"`
	Data    any          `json:"data,omitempty"`
	Error   string       `json:"error,omitempty"`
}
type DNSRecord struct {
	StaticIP bool      `json:"staticIp"`
	Hostname string    `json:"hostname"`
	RecordID string    `json:"recordId"`
	SRVID    string    `json:"srvId,omitempty"`
	Port     int       `json:"port"`
	LastIP   string    `json:"lastIp"`
	Updated  time.Time `json:"updated"`
	Error    string    `json:"error,omitempty"`
}
type PlayitState struct {
	Linked   bool             `json:"linked"`
	State    string           `json:"state"`
	Message  string           `json:"message"`
	ClaimURL string           `json:"claimUrl,omitempty"`
	Tunnels  []map[string]any `json:"tunnels,omitempty"`
}
