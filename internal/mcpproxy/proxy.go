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
		item := map[string]any{
			"name": name, "type": typeName, "state": "configured",
			"source": "merged_config",
		}
		if description := strings.TrimSpace(srv.Description); description != "" {
			item["description"] = description
		}
		if srv.IsPlugin {
			item["plugin"] = true
			item["trusted"] = srv.Trust
		}
		out = append(out, item)
	}
	return out
}

// ExpandEnv replaces ${VAR} in env map values.
func ExpandEnv(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, k+"="+os.ExpandEnv(v))
	}
	return out
}

// PingCommand checks the upstream binary is invokable (not full MCP handshake).
func (m *Manager) PingCommand(ctx context.Context, name string) error {
	srv, ok := m.servers[name]
	if !ok {
		return fmt.Errorf("server %q not configured", name)
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
	_, ok := m.servers[name]
	return ok
}

// ServerConfig returns config for name.
func (m *Manager) ServerConfig(name string) (config.MCPServer, bool) {
	if !m.enabled {
		return config.MCPServer{}, false
	}
	srv, ok := m.servers[name]
	return srv, ok
}

// DescribeCommand returns command line for logging (no secrets).
func DescribeCommand(s config.MCPServer) string {
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}
