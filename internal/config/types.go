// Package config loads, validates and resolves broker.yaml. See docs/PERMISSIONS.md for the
// meaning of agents, roles, classes and floors.
package config

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole of broker.yaml.
type Config struct {
	Server     Server               `yaml:"server"`
	Dashboard  Dashboard            `yaml:"dashboard"`
	Approvals  Approvals            `yaml:"approvals"`
	Audit      Audit                `yaml:"audit"`
	Agents     map[string]Agent     `yaml:"agents"`
	Roles      map[string]Role      `yaml:"roles"`
	Policy     Policy               `yaml:"policy"`
	Connectors map[string]Connector `yaml:"connectors"`
	Profiles   map[string]Profile   `yaml:"profiles"`
	// Groups describes the group headings connectors are filed under, for agents:
	// list_connectors shows each group's description above its connectors.
	Groups map[string]Group `yaml:"groups"`

	// Path is the file the config was loaded from; Dir is its directory, against which
	// relative paths in the config resolve.
	Path string `yaml:"-"`
	Dir  string `yaml:"-"`
}

type Server struct {
	Listen          string   `yaml:"listen"`
	AllowedSources  []Prefix `yaml:"allowed_sources"`
	FirewallProfile string   `yaml:"firewall_profile"`
	TLS             TLS      `yaml:"tls"`
	TokensFile      string   `yaml:"tokens_file"`
	MaxResultBytes  int      `yaml:"max_result_bytes"`
}

type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Dashboard struct {
	AdminFrom []Prefix `yaml:"admin_from"`
	TokenFile string   `yaml:"token_file"`
}

type Approvals struct {
	Timeout time.Duration `yaml:"timeout"`
	Notify  []string      `yaml:"notify"`
	Ntfy    Ntfy          `yaml:"ntfy"`
}

type Ntfy struct {
	URL string `yaml:"url"`
}

type Audit struct {
	Path string `yaml:"path"`
}

// Agent is a caller identity. Its token (in tokens.yaml) selects it; Match must also hold.
type Agent struct {
	Role      string    `yaml:"role"`
	Match     Match     `yaml:"match"`
	Approvals []Channel `yaml:"approvals"`
}

type Match struct {
	Process []string `yaml:"process"`
	From    []Prefix `yaml:"from"`
}

// Role decides what an agent sees and what each class of call does.
type Role struct {
	Connectors []string          `yaml:"connectors"`
	Classes    map[string]Action `yaml:"classes"`
	Profiles   map[string]Action `yaml:"profiles"`
	Rules      []Rule            `yaml:"rules"`
}

// Rule is a glob on "<connector>/<tool>" or "profile/<name>/<action>" and its action.
type Rule struct {
	Match  string `yaml:"match"`
	Action Action `yaml:"action"`
}

type Policy struct {
	Floors []Rule `yaml:"floors"`
}

// Group is what agents are told about a group of connectors.
type Group struct {
	Description string `json:"description" yaml:"description,omitempty"`
}

// Connector is a downstream MCP server: local (Command) or remote Streamable HTTP (URL).
type Connector struct {
	Description string `yaml:"description"`
	// Group is a heading the dashboard files the connector under, and list_connectors lists
	// it under; it has no effect on policy.
	Group string `yaml:"group"`
	// Aliases are other names people use for what the connector reaches (an account
	// number, a site's name), shown to agents so a request can be matched to it. They have
	// no effect on policy.
	Aliases     []string          `yaml:"aliases"`
	Tags        []string          `yaml:"tags"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Cwd         string            `yaml:"cwd"`
	Env         map[string]string `yaml:"env"`
	EnvFile     string            `yaml:"env_file"`
	URL         string            `yaml:"url"`
	Headers     map[string]string `yaml:"headers"`
	OAuth       *OAuth            `yaml:"oauth"`
	IdleUnload  time.Duration     `yaml:"idle_unload"`
	Instance    string            `yaml:"instance"`
	ToolClasses map[string]Class  `yaml:"tool_classes"`
}

// OAuth signs the broker in to a remote connector with the authorization code flow (PKCE).
// The server's metadata supplies the endpoints. Without a client_id, the broker registers
// its own client with the server's authorization server (dynamic client registration) on
// the first sign-in and reuses it after that.
type OAuth struct {
	// ClientID is a client registered by hand; empty means dynamic registration.
	ClientID string `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	// ClientSecret is only for confidential clients; reference ${NAME} from env_file.
	ClientSecret string `json:"client_secret,omitempty" yaml:"client_secret,omitempty"`
	// Scopes replaces the scopes the server advertises, which are often all of them.
	Scopes []string `json:"scopes,omitempty" yaml:"scopes,omitempty,flow"`
	// RedirectURL defaults to http://localhost:<port>/oauth/callback on this broker; it
	// must match the one registered for the client exactly.
	RedirectURL string `json:"redirect_url,omitempty" yaml:"redirect_url,omitempty"`
	// PrivateNetwork lets sign-in reach authorization servers on private, link-local or
	// CGNAT addresses, such as a self-hosted GitLab behind a VPN. Without it the MCP SDK
	// refuses them, to stop a server pointing the broker at the local network.
	PrivateNetwork bool `json:"private_network,omitempty" yaml:"private_network,omitempty"`
}

// ImportedPlaceholder starts the description older imports wrote ("Imported from
// claude-code"), which says where a connector came from, not what it does.
const ImportedPlaceholder = "Imported from "

// NeedsDescription reports whether the connector has no description worth showing an
// agent: none, or an import's placeholder.
func (c Connector) NeedsDescription() bool {
	d := strings.TrimSpace(c.Description)
	return d == "" || strings.HasPrefix(d, ImportedPlaceholder)
}

// Remote reports whether the connector is reached over HTTP rather than spawned.
func (c Connector) Remote() bool { return c.URL != "" }

// OAuthSettings is how the broker signs in to the connector: its oauth block or, for a
// remote connector with neither that nor an Authorization header, an implicit empty one
// (dynamic client registration). The implicit one only acts when the server answers 401,
// so servers that need no sign-in are unaffected. nil: the connector doesn't use OAuth.
func (c Connector) OAuthSettings() (o *OAuth, implicit bool) {
	if c.OAuth != nil {
		return c.OAuth, false
	}
	if !c.Remote() {
		return nil, false
	}
	for k := range c.Headers {
		if strings.EqualFold(k, "Authorization") {
			return nil, false
		}
	}
	return &OAuth{}, true
}

// Profile is a supervised long-running local process (ComfyUI and friends).
type Profile struct {
	Description string `yaml:"description"`
	// Group is a heading the dashboard files the profile under.
	Group       string            `yaml:"group"`
	Cwd         string            `yaml:"cwd"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	EnvFile     string            `yaml:"env_file"`
	Env         map[string]string `yaml:"env"`
	PathPrepend []string          `yaml:"path_prepend"`
	Ports       []int             `yaml:"ports"`
	// RequiresPaths must exist for start to go ahead: folders on drives that are not
	// always mounted (encrypted or removable), say.
	RequiresPaths  []string   `yaml:"requires_paths"`
	NetworkExposed bool       `yaml:"network_exposed"`
	Console        bool       `yaml:"console"`
	Detach         bool       `yaml:"detach"`
	Health         Health     `yaml:"health"`
	Stop           Stop       `yaml:"stop"`
	Update         [][]string `yaml:"update"`
}

type Health struct {
	HTTP          string        `yaml:"http"`
	Interval      time.Duration `yaml:"interval"`
	StartTimeout  time.Duration `yaml:"start_timeout"`
	LogReadyRegex string        `yaml:"log_ready_regex"`
}

type Stop struct {
	Graceful string        `yaml:"graceful"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Action is a policy outcome.
type Action string

const (
	Allow   Action = "allow"
	Approve Action = "approve"
	Deny    Action = "deny"
)

func (a Action) strictness() int {
	switch a {
	case Allow:
		return 0
	case Approve:
		return 1
	default:
		return 2 // unknown actions are treated as deny
	}
}

// Stricter returns whichever of a and b is stricter.
func Stricter(a, b Action) Action {
	if b.strictness() > a.strictness() {
		return b
	}
	return a
}

func (a *Action) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v := Action(strings.ToLower(s))
	switch v {
	case Allow, Approve, Deny:
		*a = v
		return nil
	}
	return fmt.Errorf("line %d: unknown action %q (want allow, approve or deny)", n.Line, s)
}

// Class is what kind of effect a tool call has.
type Class string

const (
	Read        Class = "read"
	Write       Class = "write"
	Destructive Class = "destructive"
	External    Class = "external"
	Spend       Class = "spend"
	Exec        Class = "exec"
)

// Classes lists every class, least to most severe.
var Classes = []Class{Read, Write, Destructive, External, Spend, Exec}

// Rank orders classes by severity; unknown classes rank above Exec.
func (c Class) Rank() int {
	for i, k := range Classes {
		if k == c {
			return i
		}
	}
	return len(Classes)
}

func (c *Class) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v := Class(strings.ToLower(s))
	if v.Rank() == len(Classes) {
		return fmt.Errorf("line %d: unknown tool class %q", n.Line, s)
	}
	*c = v
	return nil
}

// Channel is where an approval may be answered.
type Channel string

const (
	ChanElicitation Channel = "elicitation"
	ChanTray        Channel = "tray"
	ChanDashboard   Channel = "dashboard"
	ChanNtfy        Channel = "ntfy"
)

func (c *Channel) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v := Channel(strings.ToLower(s))
	switch v {
	case ChanElicitation, ChanTray, ChanDashboard, ChanNtfy:
		*c = v
		return nil
	}
	return fmt.Errorf("line %d: unknown approval channel %q", n.Line, s)
}

// ProfileActions are the keys a role's `profiles:` map may use.
var ProfileActions = []string{"status", "logs", "plan", "start", "stop", "update", "kill_foreign"}

// Prefix is a CIDR; a bare address is accepted as a single-host prefix.
type Prefix struct{ netip.Prefix }

func (p *Prefix) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParsePrefix(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*p = v
	return nil
}

// ParsePrefix parses a CIDR or a bare IP address.
func ParsePrefix(s string) (Prefix, error) {
	if pfx, err := netip.ParsePrefix(s); err == nil {
		return Prefix{pfx.Masked()}, nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return Prefix{}, fmt.Errorf("invalid CIDR or address %q", s)
	}
	return Prefix{netip.PrefixFrom(addr, addr.BitLen())}, nil
}

// ContainsAddr reports whether any prefix contains addr. IPv4-mapped IPv6 addresses
// (as Go reports IPv4 clients on dual-stack listeners) are unmapped first.
func ContainsAddr(ps []Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range ps {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Loopback is 127.0.0.0/8 and ::1.
var Loopback = []Prefix{
	{netip.MustParsePrefix("127.0.0.0/8")},
	{netip.MustParsePrefix("::1/128")},
}
