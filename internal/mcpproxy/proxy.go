package mcpproxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"mcpx/internal/config"
	"mcpx/internal/logging"
)

// Proxy manages upstream MCP stdio processes (minimal Phase: list + call via raw is complex).
// For M5 we expose configured servers and run a simple "tools/list via npx" is heavy.
// Practical approach: list from config; call spawns one-shot or reuse client from mcp-go.

// Manager is an immutable request-local view of merged MCP server configs.
type Manager struct {
	servers map[string]config.MCPServer
	enabled bool
}

// NewManager from merged MCP file.
func NewManager(enabled bool, file config.MCPFile) *Manager {
	m := &Manager{servers: map[string]config.MCPServer{}, enabled: enabled}
	for name, s := range file.MCPServers {
		m.servers[name] = s
	}
	return m
}

// List returns stable, secret-free machine-readable server descriptors.
func (m *Manager) List() []map[string]any {
	if !m.enabled {
		return []map[string]any{}
	}
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		srv := m.servers[name]
		typeName := srv.Type
		if typeName == "" {
			typeName = "stdio"
		}
		state := "configured"
		if !srv.IsEnabled() {
			state = "disabled"
		}
		source := strings.TrimSpace(srv.Source)
		if source == "" {
			source = "merged_config"
		}
		item := map[string]any{
			"name": name, "type": typeName, "state": state, "enabled": srv.IsEnabled(),
			"source": source,
		}
		if srv.Trust || srv.TrustRequested || srv.IsPlugin {
			item["trusted"] = srv.Trust
		}
		if description := strings.TrimSpace(srv.Description); description != "" {
			item["description"] = description
		}
		if srv.Source == config.MCPSourceWorkspace && srv.TrustRequested {
			item["trust_requested"] = true
			if srv.Trust {
				item["trust_state"] = "trusted"
			} else {
				item["trust_state"] = "needs_approval"
			}
		}
		if srv.IsPlugin {
			item["plugin"] = true
		}
		out = append(out, item)
	}
	return out
}

// Servers returns a copy of the effective server definitions.
func (m *Manager) Servers() map[string]config.MCPServer {
	out := make(map[string]config.MCPServer, len(m.servers))
	if !m.enabled {
		return out
	}
	for name, server := range m.servers {
		if server.IsEnabled() {
			out[name] = server
		}
	}
	return out
}

// ExpandEnv replaces ${VAR} in env map values using the process environment.
func ExpandEnv(env map[string]string) []string { return ExpandEnvWith(env, nil) }

// ExpandEnvWith additionally exposes Runtime-owned variables without mutating
// the MCPX process environment. Runtime variables win during template
// expansion; the registration may then map them to Plugin-specific env names.
func ExpandEnvWith(env, runtime map[string]string) []string {
	lookup := func(key string) string {
		if value, ok := runtime[key]; ok {
			return value
		}
		return os.Getenv(key)
	}
	var out []string
	for k, v := range env {
		out = append(out, k+"="+os.Expand(v, lookup))
	}
	return out
}

func ExpandValue(value string, runtime map[string]string) string {
	return os.Expand(value, func(key string) string {
		if resolved, ok := runtime[key]; ok {
			return resolved
		}
		return os.Getenv(key)
	})
}

// PingCommand checks the upstream binary is invokable (not full MCP handshake).
func (m *Manager) PingCommand(ctx context.Context, name string) error {
	srv, ok := m.servers[name]
	if !ok {
		return fmt.Errorf("server %q not configured", name)
	}
	if !srv.IsEnabled() {
		return fmt.Errorf("server %q is disabled", name)
	}
	if srv.Command == "" {
		return fmt.Errorf("empty command")
	}
	// best-effort: resolve look path
	_, err := exec.LookPath(srv.Command)
	if err != nil {
		// npx may still work via path later
		logging.Debug("mcp lookpath", "server", name, "err", err)
	}
	return nil
}

// Call is a placeholder that returns structured error until full MCP client wiring.
// Real call uses mcp-go client stdio — implemented in CallTool.
func (m *Manager) Has(name string) bool {
	if !m.enabled {
		return false
	}
	srv, ok := m.servers[name]
	return ok && srv.IsEnabled()
}

// ConfiguredServer returns config regardless of the registration-level enabled switch.
func (m *Manager) ConfiguredServer(name string) (config.MCPServer, bool) {
	if !m.enabled {
		return config.MCPServer{}, false
	}
	srv, ok := m.servers[name]
	return srv, ok
}

// ServerConfig returns enabled config for name.
func (m *Manager) ServerConfig(name string) (config.MCPServer, bool) {
	srv, ok := m.ConfiguredServer(name)
	if !ok || !srv.IsEnabled() {
		return config.MCPServer{}, false
	}
	return srv, true
}

// DescribeCommand returns command line for logging (no secrets).
func DescribeCommand(s config.MCPServer) string {
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}
