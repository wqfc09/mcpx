package mcpproxy

import (
	"testing"

	"mcpx/internal/config"
)

func TestManagersKeepWorkspaceConfigsIsolated(t *testing.T) {
	workspaceA := NewManager(true, config.MCPFile{MCPServers: map[string]config.MCPServer{
		"db": {Command: "db-a", Env: map[string]string{"TOKEN": "a"}},
	}})
	workspaceB := NewManager(true, config.MCPFile{MCPServers: map[string]config.MCPServer{
		"db": {Command: "db-b", Env: map[string]string{"TOKEN": "b"}},
	}})

	a, ok := workspaceA.ServerConfig("db")
	if !ok || a.Command != "db-a" || a.Env["TOKEN"] != "a" {
		t.Fatalf("workspace A config changed: %+v ok=%v", a, ok)
	}
	b, ok := workspaceB.ServerConfig("db")
	if !ok || b.Command != "db-b" || b.Env["TOKEN"] != "b" {
		t.Fatalf("workspace B config changed: %+v ok=%v", b, ok)
	}
}

func TestDisabledManagerExposesNoServers(t *testing.T) {
	m := NewManager(false, config.MCPFile{MCPServers: map[string]config.MCPServer{
		"db": {Command: "db"},
	}})
	if got := m.List(); len(got) != 0 {
		t.Fatalf("disabled list: %+v", got)
	}
	if _, ok := m.ServerConfig("db"); ok {
		t.Fatal("disabled manager returned server config")
	}
}

func TestListReturnsStableMachineReadableDescriptorsWithoutSecrets(t *testing.T) {
	m := NewManager(true, config.MCPFile{MCPServers: map[string]config.MCPServer{
		"zeta":  {Command: "zeta", Env: map[string]string{"TOKEN": "secret"}, IsPlugin: true, Trust: true, Plugin: &config.MCPPlugin{Tools: []string{"run"}, Inbox: "inbox"}},
		"alpha": {Command: "alpha"},
	}})
	items := m.List()
	if len(items) != 2 || items[0]["name"] != "alpha" || items[1]["name"] != "zeta" {
		t.Fatalf("unstable descriptors: %+v", items)
	}
	if _, exposed := items[1]["env"]; exposed {
		t.Fatalf("descriptor exposed environment: %+v", items[1])
	}
	if items[1]["plugin"] != true || items[1]["trusted"] != true {
		t.Fatalf("plugin descriptor missing trust metadata: %+v", items[1])
	}
	if items[0]["plugin"] != nil || items[0]["trusted"] != nil {
		t.Fatalf("ordinary MCP server was marked trusted: %+v", items[0])
	}
	for _, legacy := range []string{"invocation", "tool_discovery"} {
		if items[0][legacy] != nil {
			t.Fatalf("inventory descriptor must not expose incomplete %s metadata: %+v", legacy, items[0])
		}
	}
}
