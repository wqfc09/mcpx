package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfigUsesTransportSessionTTL(t *testing.T) {
	encoded, err := yaml.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "transport:\n    session_idle_ttl: 24h") {
		t.Fatalf("transport config missing:\n%s", text)
	}
	if strings.Contains(text, "\nsession:") {
		t.Fatalf("legacy session config must not be emitted:\n%s", text)
	}
}

func TestDefaultConfigUsesStateRetentionDefaults(t *testing.T) {
	cfg := DefaultConfig()
	retention := cfg.State.Retention
	if !retention.Enabled || retention.Interval != "24h" {
		t.Fatalf("retention enabled/interval: %+v", retention)
	}
	if retention.ProcessEventTTL != "720h" || retention.ProcessEventMaxRows != 10000 {
		t.Fatalf("process retention: %+v", retention)
	}
	if retention.MemoryEventTTL != "4320h" || retention.MemoryEventMaxRows != 2000 {
		t.Fatalf("memory retention: %+v", retention)
	}
	if retention.TerminalTaskTTL != "720h" || retention.SnapshotTTL != "2160h" || retention.VacuumThresholdRows != 10000 {
		t.Fatalf("secondary retention: %+v", retention)
	}
}

func TestLoadGlobalParsesAndValidatesStateRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`state:
  retention:
    enabled: false
    interval: 2h
    process_event_ttl: 48h
    process_event_max_rows: 42
    memory_event_ttl: 72h
    memory_event_max_rows: 24
    terminal_task_ttl: 96h
    snapshot_ttl: 120h
    vacuum_threshold_rows: 8
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	retention := cfg.State.Retention
	if retention.Enabled || retention.Interval != "2h" || retention.ProcessEventMaxRows != 42 || retention.VacuumThresholdRows != 8 {
		t.Fatalf("parsed retention: %+v", retention)
	}
}

func TestLoadGlobalRejectsInvalidStateRetention(t *testing.T) {
	tests := []string{
		"state:\n  retention:\n    interval: nope\n",
		"state:\n  retention:\n    process_event_ttl: -1h\n",
		"state:\n  retention:\n    process_event_max_rows: 0\n",
		"state:\n  retention:\n    vacuum_threshold_rows: -1\n",
	}
	for _, content := range tests {
		t.Run(strings.ReplaceAll(strings.TrimSpace(content), "\n", "/"), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGlobal(path); err == nil {
				t.Fatalf("expected invalid retention config to fail: %s", content)
			}
		})
	}
}

func TestMergeDoesNotAllowProjectStateRetentionOverride(t *testing.T) {
	global := DefaultConfig()
	project := Config{State: StateConfig{Retention: RetentionConfig{
		Enabled: false, Interval: "1h", ProcessEventMaxRows: 1,
	}}}
	merged := Merge(global, project)
	if merged.State.Retention != global.State.Retention {
		t.Fatalf("project changed global retention: got=%+v want=%+v", merged.State.Retention, global.State.Retention)
	}
}

func TestMergeKeepsGlobalTokenAndProjectDescription(t *testing.T) {
	g := DefaultConfig()
	g.Auth.Token = "global-tok"
	p := Config{Auth: AuthConfig{Token: "proj-tok"}, Description: "proj desc"}
	m := Merge(g, p)
	if m.Auth.Token != "global-tok" {
		t.Fatalf("token: %q", m.Auth.Token)
	}
	if m.Description != "proj desc" {
		t.Fatalf("desc: %q", m.Description)
	}
}

func TestGlobalSystemPromptPathUsesMCPXHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	path, err := GlobalSystemPromptPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "system_prompt.md") {
		t.Fatalf("system prompt path: %q", path)
	}
}

func TestMergeExplicitFalseFeatureFlags(t *testing.T) {
	global := DefaultConfig()
	var project Config
	if err := yaml.Unmarshal([]byte(`
terminal:
  enabled: false
file_watch:
  enabled: false
discovery:
  mcp:
    enabled: false
  skills:
    enabled: false
logging:
  enabled: false
`), &project); err != nil {
		t.Fatal(err)
	}
	merged := Merge(global, project)
	if merged.Terminal.Enabled || merged.FileWatch.Enabled ||
		merged.Discovery.MCP.Enabled || merged.Discovery.Skills.Enabled || merged.Logging.Enabled {
		t.Fatalf("explicit false flags not preserved: %+v", merged)
	}
}

func TestValidateSecurityRulesRejectsInvalidRegexp(t *testing.T) {
	err := ValidateSecurityRules(SecurityConfig{Commands: CommandRules{Deny: []string{"["}}})
	if err == nil {
		t.Fatal("expected invalid regexp error")
	}
}

func TestMergeCommandsReplace(t *testing.T) {
	g := DefaultConfig()
	p := Config{
		Security: SecurityConfig{
			Commands: CommandRules{Allow: []string{`^echo`}},
		},
	}
	m := Merge(g, p)
	if len(m.Security.Commands.Allow) != 1 || m.Security.Commands.Allow[0] != `^echo` {
		t.Fatalf("allow: %+v", m.Security.Commands.Allow)
	}
	if len(m.Security.Commands.Deny) != 0 {
		t.Fatalf("deny should be replaced empty, got %+v", m.Security.Commands.Deny)
	}
}

func TestMergeMCP(t *testing.T) {
	g := MCPFile{MCPServers: map[string]MCPServer{
		"github": {Command: "g", Type: "stdio"},
	}}
	p := MCPFile{MCPServers: map[string]MCPServer{
		"github": {Command: "g2", Type: "stdio"},
		"local":  {Command: "l", Type: "stdio"},
	}}
	m := MergeMCP(g, p)
	if m.MCPServers["github"].Command != "g2" {
		t.Fatal("project should override")
	}
	if _, ok := m.MCPServers["local"]; !ok {
		t.Fatal("project add")
	}
}

func TestLoadMergedMCPUsesActivationOnlyForGlobalRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := t.TempDir()
	if err := WriteMCPFile(filepath.Join(home, ".mcp.json"), MCPFile{MCPServers: map[string]MCPServer{
		"shared": {Command: "global", Args: []string{"serve"}, Trust: true, InjectInstructions: true},
	}}); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err := WriteMCPFile(ProjectMCPPath(workspace), MCPFile{MCPServers: map[string]MCPServer{
		"shared": {Enabled: &disabled},
	}}); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadMergedMCP(workspace)
	if err != nil {
		t.Fatal(err)
	}
	got := merged.MCPServers["shared"]
	if got.Command != "global" || got.Source != MCPSourceGlobal || !got.Trust || !got.InjectInstructions || got.IsEnabled() {
		t.Fatalf("Workspace activation changed Global definition: %+v", got)
	}
}

func TestLoadMergedMCPRejectsRedefinitionOfGlobalOrdinaryMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := t.TempDir()
	if err := WriteMCPFile(filepath.Join(home, ".mcp.json"), MCPFile{MCPServers: map[string]MCPServer{
		"shared": {Command: "global"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPFile(ProjectMCPPath(workspace), MCPFile{MCPServers: map[string]MCPServer{
		"shared": {Command: "workspace"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMergedMCP(workspace); err == nil || !strings.Contains(err.Error(), "only enabled may be overridden") {
		t.Fatalf("Global MCP redefinition should be rejected, err=%v", err)
	}
}

func TestLoadMergedMCPRejectsWorkspacePluginIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := t.TempDir()
	if err := WriteMCPFile(ProjectMCPPath(workspace), MCPFile{MCPServers: map[string]MCPServer{
		"local": {Command: "local", IsPlugin: true, Trust: true, Plugin: &MCPPlugin{Tools: []string{}, Inbox: "inbox"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMergedMCP(workspace); err == nil || !strings.Contains(err.Error(), "cannot declare Plugin identity") {
		t.Fatalf("Workspace Plugin identity should be rejected, err=%v", err)
	}
}

func TestLoadMergedMCPAllowsGlobalPluginActivationButRejectsRedefinition(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := t.TempDir()
	if err := WriteMCPFile(filepath.Join(home, ".mcp.json"), MCPFile{MCPServers: map[string]MCPServer{
		"plugin": {Command: "global", IsPlugin: true, Plugin: &MCPPlugin{Tools: []string{}, Inbox: "inbox"}},
	}}); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := WriteMCPFile(ProjectMCPPath(workspace), MCPFile{MCPServers: map[string]MCPServer{
		"plugin": {Enabled: &enabled},
	}}); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadMergedMCP(workspace)
	if err != nil {
		t.Fatal(err)
	}
	got := merged.MCPServers["plugin"]
	if !got.IsPlugin || got.Command != "global" || !got.IsEnabled() {
		t.Fatalf("Global Plugin activation=%+v", got)
	}
	if err := WriteMCPFile(ProjectMCPPath(workspace), MCPFile{MCPServers: map[string]MCPServer{
		"plugin": {Command: "workspace"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMergedMCP(workspace); err == nil || !strings.Contains(err.Error(), "only enabled may be overridden") {
		t.Fatalf("Global Plugin redefinition should be rejected, err=%v", err)
	}
}
func TestMCPRegistrationFingerprintIgnoresNonTrustFields(t *testing.T) {
	base := MCPServer{Command: "node", Args: []string{"server.js"}, Trust: true, Description: "one", Env: map[string]string{"TOKEN": "a"}}
	baseline := MCPRegistrationFingerprint(base)
	disabled := false
	variant := base
	variant.Enabled = &disabled
	variant.Trust = false
	variant.Description = "two"
	variant.Env = map[string]string{"TOKEN": "b"}
	if got := MCPRegistrationFingerprint(variant); got != baseline {
		t.Fatalf("enabled/trust/description/env changed fingerprint: %s != %s", got, baseline)
	}
	variant = base
	variant.Args = []string{"server.js", "--danger"}
	if got := MCPRegistrationFingerprint(variant); got == baseline {
		t.Fatal("args change must invalidate fingerprint")
	}
	variant = base
	variant.InjectInstructions = true
	if got := MCPRegistrationFingerprint(variant); got == baseline {
		t.Fatal("injectInstructions change must invalidate fingerprint")
	}
}

func TestValidateMCPFileRejectsInvalidPluginContract(t *testing.T) {
	tests := []struct {
		name   string
		server MCPServer
	}{
		{name: "missing plugin", server: MCPServer{IsPlugin: true}},
		{name: "missing explicit tools", server: MCPServer{IsPlugin: true, Plugin: &MCPPlugin{Inbox: "inbox"}}},
		{name: "missing inbox", server: MCPServer{IsPlugin: true, Plugin: &MCPPlugin{Tools: []string{"run"}}}},
		{name: "tool wildcard", server: MCPServer{IsPlugin: true, Plugin: &MCPPlugin{Tools: []string{"*"}, Inbox: "inbox"}}},
		{name: "inbox wildcard", server: MCPServer{IsPlugin: true, Plugin: &MCPPlugin{Tools: []string{"run"}, Inbox: "*"}}},
		{name: "public inbox", server: MCPServer{IsPlugin: true, Plugin: &MCPPlugin{Tools: []string{"inbox"}, Inbox: "inbox"}}},
		{name: "plugin without identity", server: MCPServer{Plugin: &MCPPlugin{Tools: []string{}, Inbox: "inbox"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"demo": test.server}}); err == nil {
				t.Fatalf("invalid Plugin config was accepted: %+v", test.server)
			}
		})
	}
}
