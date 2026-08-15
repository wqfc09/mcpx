package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/mcpproxy"
	"mcpx/internal/workspace"
)

type pluginRuntimeLease struct {
	Key           string
	Plugin        string
	Scope         string
	WorkspaceID   string
	WorkspaceName string
	WorkspacePath string
	RuntimeDir    string
	Server        config.MCPServer
	Client        *mcpproxy.ClientSession
	StartedAt     time.Time
	LastUsedAt    time.Time
}

type pluginRuntimeManager struct {
	home       string
	instanceID string
	mu         sync.Mutex
	leases     map[string]*pluginRuntimeLease
}

func newPluginRuntimeManager(home, instanceID string) *pluginRuntimeManager {
	return &pluginRuntimeManager{home: home, instanceID: instanceID, leases: map[string]*pluginRuntimeLease{}}
}

// ProbeCatalog reads the Global Plugin schema without creating a reusable
// business lease. A Plugin definition must support initialize/tools-list in
// catalog mode without Workspace-local state so MCPX tools/list remains stable
// across Workspace activation changes.
func (m *pluginRuntimeManager) ProbeCatalog(ctx context.Context, pluginName string, server config.MCPServer) ([]*mcp.Tool, error) {
	if m == nil {
		return nil, fmt.Errorf("Plugin Runtime Manager is unavailable")
	}
	if server.Plugin == nil || !server.IsPlugin || server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return nil, fmt.Errorf("Plugin %q does not use the MCP runtime", pluginName)
	}
	runtimeDir := filepath.Join(m.home, "runtime", "plugins", pluginName, "catalog")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare Plugin catalog runtime %q: %w", runtimeDir, err)
	}
	probe := server
	probe.Enabled = nil
	probe.WorkDir = m.home
	probe.RuntimeEnv = pluginRuntimeEnv(m.instanceID, m.home, pluginName, server.Plugin.RuntimeScope(), runtimeDir, workspace.Workspace{})
	probe.RuntimeEnv["MCPX_PLUGIN_CATALOG"] = "1"
	client, err := mcpproxy.OpenClientSession(ctx, probe, nil)
	if err != nil {
		return nil, fmt.Errorf("start Plugin %q catalog probe: %w", pluginName, err)
	}
	defer client.Close()
	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("Plugin %q catalog tools/list: %w", pluginName, err)
	}
	return tools, nil
}

func (m *pluginRuntimeManager) Ensure(ctx context.Context, pluginName string, server config.MCPServer, ws workspace.Workspace) (*pluginRuntimeLease, []*mcp.Tool, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("Plugin Runtime Manager is unavailable")
	}
	if server.Plugin == nil || !server.IsPlugin || server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return nil, nil, fmt.Errorf("Plugin %q does not use the MCP runtime", pluginName)
	}
	if !server.IsEnabled() {
		return nil, nil, fmt.Errorf("Plugin %q is disabled for this Workspace", pluginName)
	}
	scope := server.Plugin.RuntimeScope()
	if scope == config.PluginScopeWorkspace && strings.TrimSpace(ws.Path) == "" {
		return nil, nil, fmt.Errorf("workspace-scoped Plugin %q requires a Workspace", pluginName)
	}
	key := pluginLeaseKey(pluginName, scope, ws)

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.leases[key]; existing != nil && existing.Client != nil {
		tools, err := existing.Client.ListTools(ctx)
		if err == nil {
			existing.LastUsedAt = time.Now().UTC()
			return existing, tools, nil
		}
		existing.Client.Close()
		delete(m.leases, key)
	}

	runtimeDir := m.runtimeDir(pluginName, scope, ws)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("prepare Plugin runtime %q: %w", runtimeDir, err)
	}
	runtimeServer := server
	extraRuntimeEnv := runtimeServer.RuntimeEnv
	runtimeServer.RuntimeEnv = map[string]string{}
	for key, value := range extraRuntimeEnv {
		runtimeServer.RuntimeEnv[key] = value
	}
	// MCPX-owned launch context always wins over registration/runtime extras.
	for key, value := range pluginRuntimeEnv(m.instanceID, m.home, pluginName, scope, runtimeDir, ws) {
		runtimeServer.RuntimeEnv[key] = value
	}
	if scope == config.PluginScopeWorkspace {
		runtimeServer.WorkDir = ws.Path
	} else if strings.TrimSpace(runtimeServer.WorkDir) == "" {
		runtimeServer.WorkDir = m.home
	}
	client, err := mcpproxy.OpenClientSession(ctx, runtimeServer, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("start Plugin %q %s lease: %w", pluginName, scope, err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("Plugin %q tools/list: %w", pluginName, err)
	}
	now := time.Now().UTC()
	lease := &pluginRuntimeLease{
		Key: key, Plugin: pluginName, Scope: scope, WorkspaceID: ws.ID,
		WorkspaceName: ws.Name, WorkspacePath: ws.Path, RuntimeDir: runtimeDir,
		Server: runtimeServer, Client: client, StartedAt: now, LastUsedAt: now,
	}
	m.leases[key] = lease
	return lease, tools, nil
}

func (m *pluginRuntimeManager) Invalidate(key string) {
	if m == nil || strings.TrimSpace(key) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease := m.leases[key]; lease != nil {
		if lease.Client != nil {
			lease.Client.Close()
		}
		delete(m.leases, key)
	}
}

func (m *pluginRuntimeManager) State(pluginName, scope string, ws workspace.Workspace) map[string]any {
	if m == nil {
		return map[string]any{"state": "unavailable"}
	}
	key := pluginLeaseKey(pluginName, scope, ws)
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[key]
	if lease == nil {
		return map[string]any{"state": "configured", "lease_key": key}
	}
	data := map[string]any{
		"state": "running", "lease_key": key, "runtime_dir": lease.RuntimeDir,
		"started_at": lease.StartedAt.Format(time.RFC3339Nano), "last_used_at": lease.LastUsedAt.Format(time.RFC3339Nano),
	}
	if lease.WorkspaceID != "" {
		data["workspace_id"] = lease.WorkspaceID
	}
	return data
}

func (m *pluginRuntimeManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.leases))
	for key := range m.leases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if client := m.leases[key].Client; client != nil {
			client.Close()
		}
		delete(m.leases, key)
	}
}

func pluginLeaseKey(pluginName, scope string, ws workspace.Workspace) string {
	if scope == config.PluginScopeWorkspace {
		return "workspace:" + ws.ID + ":" + pluginName
	}
	return "instance:" + pluginName
}

func (m *pluginRuntimeManager) runtimeDir(pluginName, scope string, ws workspace.Workspace) string {
	tail := "instance"
	if scope == config.PluginScopeWorkspace {
		tail = ws.ID
	}
	return filepath.Join(m.home, "runtime", "plugins", pluginName, tail)
}

func pluginRuntimeEnv(instanceID, home, pluginName, scope, runtimeDir string, ws workspace.Workspace) map[string]string {
	env := map[string]string{
		"MCPX_INSTANCE_ID":        instanceID,
		"MCPX_INSTANCE_HOME":      home,
		"MCPX_PLUGIN_NAME":        pluginName,
		"MCPX_PLUGIN_SCOPE":       scope,
		"MCPX_PLUGIN_RUNTIME_DIR": runtimeDir,
	}
	if scope == config.PluginScopeWorkspace {
		env["MCPX_WORKSPACE"] = ws.Path
		env["MCPX_WORKSPACE_ID"] = ws.ID
		env["MCPX_WORKSPACE_NAME"] = ws.Name
	}
	return env
}
