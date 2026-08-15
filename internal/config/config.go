package config

// Config is the Runtime YAML schema (global + project merge result).
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Auth       AuthConfig       `yaml:"auth"`
	Security   SecurityConfig   `yaml:"security"`
	State      StateConfig      `yaml:"state"`
	Workspaces []WorkspaceEntry `yaml:"workspaces"`
	Terminal   TerminalConfig   `yaml:"terminal"`
	FileWatch  FileWatchConfig  `yaml:"file_watch"`
	Discovery  DiscoveryConfig  `yaml:"discovery"`
	Logging    LoggingConfig    `yaml:"logging"`
	Transport  TransportConfig  `yaml:"transport"`
	Limits     LimitsConfig     `yaml:"limits"`
	// Project-only fields merged into effective view:
	Description string `yaml:"description,omitempty"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// DisableLocalhostProtection turns off MCP DNS-rebinding Host checks.
	// Required when clients hit a public IP/domain while the process accepts
	// the connection on loopback (reverse proxy, frp, ssh -L, etc.).
	// Prefer pairing with auth.token when enabled.
	DisableLocalhostProtection bool `yaml:"disable_localhost_protection"`
	// TrustProxyHeaders uses X-Forwarded-Proto/Host when building public URLs.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// AllowedOrigins for CORS; empty means reflect request Origin when present (dev-friendly).
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// AuthConfig controls HTTP/MCP credential checks.
// Mode: open | bearer | oauth | dual. Empty mode: bearer if Token set, else open.
type AuthConfig struct {
	Mode  string      `yaml:"mode"`
	Token string      `yaml:"token"`
	OAuth OAuthConfig `yaml:"oauth"`
}

// OAuthConfig is the in-process authorization server settings.
type OAuthConfig struct {
	Password     string   `yaml:"password"`
	ServerURL    string   `yaml:"server_url"`   // public origin, no trailing slash
	TokenTTL     int      `yaml:"token_ttl"`    // seconds
	TokenSecret  string   `yaml:"token_secret"` // hex-encoded HMAC key
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	RedirectURIs []string `yaml:"redirect_uris"`
}

type TransportConfig struct {
	SessionIdleTTL string `yaml:"session_idle_ttl"` // Go duration, e.g. "1h"
}

type LimitsConfig struct {
	MaxResultBytes int `yaml:"max_result_bytes"`
}

// StateConfig controls durable state retention. It is process-wide and is
// intentionally not overridable by a project-level .mcpx.yaml.
type StateConfig struct {
	Retention RetentionConfig `yaml:"retention"`
}

// RetentionConfig contains conservative cleanup thresholds for the state DB.
// The EnabledSet and *Set fields distinguish omitted YAML fields from an
// explicit zero/false value while merging the global configuration.
type RetentionConfig struct {
	Enabled             bool   `yaml:"enabled"`
	EnabledSet          bool   `yaml:"-"`
	Interval            string `yaml:"interval"`
	IntervalSet         bool   `yaml:"-"`
	ProcessEventTTL     string `yaml:"process_event_ttl"`
	ProcessEventTTLSet  bool   `yaml:"-"`
	ProcessEventMaxRows int    `yaml:"process_event_max_rows"`
	ProcessEventMaxSet  bool   `yaml:"-"`
	MemoryEventTTL      string `yaml:"memory_event_ttl"`
	MemoryEventTTLSet   bool   `yaml:"-"`
	MemoryEventMaxRows  int    `yaml:"memory_event_max_rows"`
	MemoryEventMaxSet   bool   `yaml:"-"`
	TerminalTaskTTL     string `yaml:"terminal_task_ttl"`
	TerminalTaskTTLSet  bool   `yaml:"-"`
	SnapshotTTL         string `yaml:"snapshot_ttl"`
	SnapshotTTLSet      bool   `yaml:"-"`
	VacuumThresholdRows int    `yaml:"vacuum_threshold_rows"`
	VacuumThresholdSet  bool   `yaml:"-"`
}

type SecurityConfig struct {
	Commands CommandRules `yaml:"commands"`
	Files    FileRules    `yaml:"files"`
}

type CommandRules struct {
	// Default is the decision when no allow/confirm/deny regex matches.
	// Values: "allow" | "confirm" | "deny". Empty => "confirm" (safe built-in).
	Default string   `yaml:"default"`
	Allow   []string `yaml:"allow"`
	Confirm []string `yaml:"confirm"`
	Deny    []string `yaml:"deny"`
	// AutoAllowReadonly enables the built-in read-only command whitelist
	// (git status/diff/log/show, ls, cat, pwd, head, tail). nil or true enables
	// it; false restores the default confirm decision. Explicit deny/confirm
	// rules always take precedence.
	AutoAllowReadonly *bool `yaml:"auto_allow_readonly"`
}

type FileRules struct {
	MaxReadBytes  int64    `yaml:"max_read_bytes"`
	MaxPatchFiles int      `yaml:"max_patch_files"`
	MaxPatchLines int      `yaml:"max_patch_lines"`
	Allow         []string `yaml:"allow"`
	Confirm       []string `yaml:"confirm"`
	Deny          []string `yaml:"deny"`
}

type WorkspaceEntry struct {
	Name        string `yaml:"name"`
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
}

type TerminalConfig struct {
	Enabled    bool `yaml:"enabled"`
	EnabledSet bool `yaml:"-"`
}

type FileWatchConfig struct {
	Enabled    bool `yaml:"enabled"`
	EnabledSet bool `yaml:"-"`
}

type DiscoveryConfig struct {
	MCP    MCPDiscovery    `yaml:"mcp"`
	Skills SkillsDiscovery `yaml:"skills"`
}

type MCPDiscovery struct {
	Enabled    bool `yaml:"enabled"`
	EnabledSet bool `yaml:"-"`
}

type SkillsDiscovery struct {
	Enabled    bool     `yaml:"enabled"`
	EnabledSet bool     `yaml:"-"`
	Dirs       []string `yaml:"dirs"`
	ExtraDirs  []string `yaml:"extra_dirs"`
}

type LoggingConfig struct {
	Enabled    bool   `yaml:"enabled"`
	EnabledSet bool   `yaml:"-"`
	Dir        string `yaml:"dir"`
}

const (
	MCPSourceGlobal    = "global"
	MCPSourceWorkspace = "workspace"

	PluginScopeInstance  = "instance"
	PluginScopeWorkspace = "workspace"

	PluginRuntimeMCP        = "mcp"
	PluginRuntimeController = "controller"

	PluginSubscriptionInbox = "inbox"

	PluginSubscriptionScopeWorkspace = "workspace"
	PluginSubscriptionScopeSessions  = "sessions"

	// PluginContributionHardMaxBytes keeps injected guidance deliberately small.
	// A target slot can choose a smaller limit.
	PluginContributionHardMaxBytes = 4096
)

// MCPFile is ~/.mcpx/.mcp.json or <workspace>/.mcpx/.mcp.json.
type MCPFile struct {
	MCPServers map[string]MCPServer `json:"mcpServers"`
}

// MCPPlugin is the Global authority for an MCPX Plugin. Workspace overlays can
// activate a same-name Plugin but cannot redefine its runtime, dependencies,
// mounted capabilities or contribution contract.
type MCPPlugin struct {
	Runtime       string                      `json:"runtime,omitempty"`
	Scope         string                      `json:"scope,omitempty"`
	Tools         []string                    `json:"tools"`
	Inbox         string                      `json:"inbox"`
	Depends       []string                    `json:"depends,omitempty"`
	Mounts        map[string]MCPPluginMount   `json:"mounts,omitempty"`
	Subscriptions []MCPPluginSubscription     `json:"subscriptions,omitempty"`
	Contributes   []MCPPluginContribution     `json:"contributes,omitempty"`
	Accepts       []MCPPluginContributionSlot `json:"accepts,omitempty"`
}

// RuntimeType defaults to MCP so existing Plugin definitions remain the native
// MCP runtime without carrying an extra field.
func (p *MCPPlugin) RuntimeType() string {
	if p == nil || p.Runtime == "" {
		return PluginRuntimeMCP
	}
	return p.Runtime
}

// RuntimeScope defaults to instance for MCP Plugins. Controller Plugins are
// validated as workspace-scoped in V1 because their state and subscriptions are
// intentionally tied to one Workspace.
func (p *MCPPlugin) RuntimeScope() string {
	if p == nil || p.Scope == "" {
		if p != nil && p.RuntimeType() == PluginRuntimeController {
			return PluginScopeWorkspace
		}
		return PluginScopeInstance
	}
	return p.Scope
}

// MCPPluginMount gives a Controller a stable local alias for one public tool of
// a dependency. Automatic must be explicit before the Controller may call the
// mount without returning control to the owner model.
type MCPPluginMount struct {
	Plugin    string                          `json:"plugin"`
	Tool      string                          `json:"tool"`
	Automatic bool                            `json:"automatic,omitempty"`
	Guards    map[string]MCPPluginStringGuard `json:"guards,omitempty"`
}

// MCPPluginStringGuard constrains one string tool argument before an automatic
// Controller mount call reaches the dependency. Exactly one rule is allowed in
// V1. This lets Global policy narrow broad domain tools such as action routers.
type MCPPluginStringGuard struct {
	Equals string   `json:"equals,omitempty"`
	Prefix string   `json:"prefix,omitempty"`
	OneOf  []string `json:"one_of,omitempty"`
}

// MCPPluginSubscription declares semantic events a Controller wants MCPX to
// deliver. V1 supports dependency Plugin inbox events only.
type MCPPluginSubscription struct {
	Plugin string `json:"plugin"`
	Kind   string `json:"kind"`
	Scope  string `json:"scope,omitempty"`
}

// MCPPluginContribution is immutable guidance supplied by one Plugin to an
// extension slot accepted by another Plugin. Path is a trusted Global asset;
// MCPX reads, bounds, hashes and pins the content when the target lease starts.
type MCPPluginContribution struct {
	Plugin string `json:"plugin"`
	Slot   string `json:"slot"`
	Path   string `json:"path"`
}

// MCPPluginContributionSlot opts a Plugin into a named contribution point.
// MaxBytes defaults to PluginContributionHardMaxBytes when omitted and may only
// reduce that hard limit.
type MCPPluginContributionSlot struct {
	Slot     string `json:"slot"`
	Skill    string `json:"skill,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

func (s MCPPluginContributionSlot) EffectiveMaxBytes() int {
	if s.MaxBytes <= 0 {
		return PluginContributionHardMaxBytes
	}
	return s.MaxBytes
}

// MCPServer describes an upstream MCP process.
type MCPServer struct {
	Type               string            `json:"type"`
	Description        string            `json:"description,omitempty"`
	Command            string            `json:"command"`
	Args               []string          `json:"args"`
	Env                map[string]string `json:"env"`
	Enabled            *bool             `json:"enabled,omitempty"`
	IsPlugin           bool              `json:"isPlugin"`
	Trust              bool              `json:"trust"`
	InjectInstructions bool              `json:"injectInstructions"`
	Plugin             *MCPPlugin        `json:"plugin,omitempty"`

	Source           string            `json:"-"`
	TrustRequested   bool              `json:"-"`
	TrustFingerprint string            `json:"-"`
	WorkDir          string            `json:"-"`
	RuntimeEnv       map[string]string `json:"-"`
}

// IsEnabled defaults to true when enabled is omitted.
func (s MCPServer) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// DefaultConfig returns built-in defaults per PRD.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 9090},
		Auth: AuthConfig{
			Mode:  "",
			Token: "",
			OAuth: OAuthConfig{TokenTTL: 86400},
		},
		Transport: TransportConfig{SessionIdleTTL: "24h"},
		Limits:    LimitsConfig{MaxResultBytes: 256 << 10},
		State: StateConfig{Retention: RetentionConfig{
			Enabled:             true,
			Interval:            "24h",
			ProcessEventTTL:     "720h",
			ProcessEventMaxRows: 10000,
			MemoryEventTTL:      "4320h",
			MemoryEventMaxRows:  2000,
			TerminalTaskTTL:     "720h",
			SnapshotTTL:         "2160h",
			VacuumThresholdRows: 10000,
		}},
		Security: SecurityConfig{
			Commands: CommandRules{
				Default: "allow",
				Allow: []string{
					`^ls\b`,
					`^pwd$`,
					`^git status`,
					`^git diff`,
					`^npm test`,
					`^echo\b`,
				},
				Confirm: []string{
					`^git push`,
					`^docker`,
					`^npm install`,
				},
				Deny: []string{
					`^rm -rf /`,
					`^mkfs`,
					`^shutdown`,
				},
			},
			Files: FileRules{
				MaxReadBytes:  1 << 20,
				MaxPatchFiles: 20,
				MaxPatchLines: 2000,
			},
		},
		Workspaces: nil,
		Terminal:   TerminalConfig{Enabled: true},
		FileWatch:  FileWatchConfig{Enabled: true},
		Discovery: DiscoveryConfig{
			MCP: MCPDiscovery{Enabled: true},
			Skills: SkillsDiscovery{
				Enabled: true,
				Dirs: []string{
					"~/.mcpx/skills",
					".mcpx/skills",
					"~/.agents/skills",
					"~/.agent/skills",
					"~/.codex/skills",
					"~/.grok/skills",
					".skills",
				},
			},
		},
		Logging: LoggingConfig{
			Enabled: true,
			Dir:     "", // empty => {MCPX_HOME|~/.mcpx}/logs at runtime
		},
	}
}
