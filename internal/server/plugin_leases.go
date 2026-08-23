package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/logging"
	"mcpx/internal/mcpproxy"
	"mcpx/internal/pluginstate"
	"mcpx/internal/workspace"
)

type pluginRuntimeLease struct {
	Key              string
	Plugin           string
	Scope            string
	WorkspaceID      string
	WorkspaceName    string
	WorkspacePath    string
	RuntimeDir       string
	RuntimeRevision  string
	Generation       string
	Server           config.MCPServer
	Client           *mcpproxy.ClientSession
	Tools            []*mcp.Tool
	StartedAt        time.Time
	LastUsedAt       time.Time
	Holders          map[string]bool
	LifecycleToken   string
	ReclaimRequested bool
	Closing          bool
	closeOnce        sync.Once
}

type pluginRuntimeManager struct {
	home       string
	instanceID string
	lifecycle  *lifecycleManager
	mu         sync.Mutex
	leases     map[string]*pluginRuntimeLease

	startMu sync.Mutex
	starts  map[string]*pluginStartLock
}

type pluginStartLock struct {
	mu   sync.Mutex
	refs int
}

func newPluginRuntimeManager(home, instanceID string) *pluginRuntimeManager {
	return &pluginRuntimeManager{
		home: home, instanceID: instanceID,
		leases: map[string]*pluginRuntimeLease{}, starts: map[string]*pluginStartLock{},
	}
}

func (m *pluginRuntimeManager) acquireStartLock(key string) func() {
	m.startMu.Lock()
	lock := m.starts[key]
	if lock == nil {
		lock = &pluginStartLock{}
		m.starts[key] = lock
	}
	lock.refs++
	m.startMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.startMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.starts[key] == lock {
			delete(m.starts, key)
		}
		m.startMu.Unlock()
	}
}

func (m *pluginRuntimeManager) Acquire(ctx context.Context, holder, pluginName string, server config.MCPServer, ws workspace.Workspace) (*pluginRuntimeLease, []*mcp.Tool, error) {
	holder = strings.TrimSpace(holder)
	if holder == "" {
		return nil, nil, fmt.Errorf("Plugin %q lifecycle holder is required", pluginName)
	}
	for attempt := 0; attempt < 2; attempt++ {
		lease, tools, err := m.Ensure(ctx, pluginName, server, ws)
		if err != nil {
			return nil, nil, err
		}
		if m.bindHolder(lease, holder) {
			return lease, tools, nil
		}
	}
	return nil, nil, fmt.Errorf("Plugin %q lease changed during acquire", pluginName)
}

func (m *pluginRuntimeManager) bindHolder(lease *pluginRuntimeLease, holder string) bool {
	if m == nil || lease == nil || strings.TrimSpace(holder) == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.leases[lease.Key]
	if current != lease || current.Closing {
		return false
	}
	if current.Holders == nil {
		current.Holders = map[string]bool{}
	}
	current.Holders[holder] = true
	current.ReclaimRequested = false
	current.LastUsedAt = time.Now().UTC()
	return true
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

	runtimeRevision := config.PluginRuntimeRevision(server)
	unlockStart := m.acquireStartLock(key)
	defer unlockStart()

	m.mu.Lock()
	existing := m.leases[key]
	reusable := existing != nil && existing.Client != nil && !existing.Client.IsClosed() && !existing.Closing && existing.RuntimeRevision == runtimeRevision && (scope != config.PluginScopeWorkspace || existing.WorkspacePath == ws.Path)
	if !reusable && existing != nil {
		existing.Closing = true
	}
	m.mu.Unlock()

	if reusable {
		// Lease health and live Tool schemas belong to the Plugin connection, not
		// to the caller request. Probe with an independent bounded context so a
		// canceled caller cannot kill a healthy shared runtime. A failed probe
		// also closes the small IsClosed()/session.Wait scheduling window.
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), pluginLeaseProbeTimeout)
		tools, probeErr := existing.Client.ListTools(probeCtx)
		cancelProbe()

		m.mu.Lock()
		if m.leases[key] != existing || existing.Closing {
			m.mu.Unlock()
			existing = nil
		} else if probeErr == nil {
			existing.Tools = tools
			existing.LastUsedAt = time.Now().UTC()
			m.mu.Unlock()
			return existing, tools, nil
		} else {
			existing.Closing = true
			m.mu.Unlock()
		}
	}

	if existing != nil {
		m.closeLease(existing, "replaced")
	}

	runtimeDir := m.runtimeDir(pluginName, scope, ws)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("prepare Plugin runtime %q: %w", runtimeDir, err)
	}
	if existing == nil {
		if err := recoverPersistedPluginLease(runtimeDir, pluginName, scope, ws, server); err != nil {
			return nil, nil, fmt.Errorf("recover stale Plugin %q lease: %w", pluginName, err)
		}
	}
	generation, err := newLifecycleID("pg")
	if err != nil {
		return nil, nil, fmt.Errorf("create Plugin %q generation: %w", pluginName, err)
	}
	controlSocket, lifecycleToken, err := m.lifecycle.PreparePluginPeer(key, pluginName, generation)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare Plugin %q lifecycle: %w", pluginName, err)
	}
	lifecycleOwned := lifecycleToken != ""
	defer func() {
		if lifecycleOwned && m.lifecycle != nil {
			m.lifecycle.CancelPluginPeer(lifecycleToken)
		}
	}()

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
	addPluginLifecycleEnv(runtimeServer.RuntimeEnv, controlSocket, key, generation, lifecycleToken)
	if scope == config.PluginScopeWorkspace {
		runtimeServer.WorkDir = ws.Path
	} else if strings.TrimSpace(runtimeServer.WorkDir) == "" {
		runtimeServer.WorkDir = m.home
	}
	client, err := mcpproxy.OpenMultiplexedClientSession(ctx, runtimeServer)
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
		WorkspaceName: ws.Name, WorkspacePath: ws.Path, RuntimeDir: runtimeDir, RuntimeRevision: runtimeRevision, Generation: generation,
		Server: runtimeServer, Client: client, Tools: tools, StartedAt: now, LastUsedAt: now, Holders: map[string]bool{},
		LifecycleToken: lifecycleToken,
	}
	if err := pluginstate.Write(runtimeDir, pluginstate.Status{
		Plugin: pluginName, Runtime: config.PluginRuntimeMCP, Scope: scope,
		WorkspaceID: ws.ID, WorkspaceName: ws.Name, PID: client.ProcessID(), Executable: client.ProcessExecutable(), Argv0: client.ProcessArgv0(), Args: append([]string{}, runtimeServer.Args...),
		StartedAt: now.Format(time.RFC3339Nano), RuntimeRevision: runtimeRevision, Generation: generation,
	}); err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("persist Plugin %q lease status: %w", pluginName, err)
	}
	m.mu.Lock()
	if existing != nil {
		if m.leases[key] != existing {
			m.mu.Unlock()
			client.Close()
			_ = pluginstate.Remove(runtimeDir)
			return nil, nil, fmt.Errorf("Plugin %q lease was invalidated during replacement", pluginName)
		}
		for holder := range existing.Holders {
			lease.Holders[holder] = true
		}
	} else if current := m.leases[key]; current != nil {
		m.mu.Unlock()
		client.Close()
		_ = pluginstate.Remove(runtimeDir)
		return nil, nil, fmt.Errorf("Plugin %q lease changed during startup", pluginName)
	}
	m.leases[key] = lease
	m.mu.Unlock()
	lifecycleOwned = false
	return lease, tools, nil
}

func (m *pluginRuntimeManager) ReleaseHolder(holder string) int {
	if m == nil || strings.TrimSpace(holder) == "" {
		return 0
	}
	holder = strings.TrimSpace(holder)
	m.mu.Lock()
	closing := make([]*pluginRuntimeLease, 0)
	for key, lease := range m.leases {
		if lease == nil || lease.Holders == nil || !lease.Holders[holder] {
			continue
		}
		delete(lease.Holders, holder)
		if (len(lease.Holders) == 0 || (lease.ReclaimRequested && !hasBlockingPluginHolders(lease.Holders))) && !lease.Closing {
			lease.Closing = true
			delete(m.leases, key)
			closing = append(closing, lease)
		}
	}
	m.mu.Unlock()
	for _, lease := range closing {
		m.closeLease(lease, "idle")
	}
	return len(closing)
}

func (m *pluginRuntimeManager) Invalidate(key string) {
	if m == nil || strings.TrimSpace(key) == "" {
		return
	}
	m.mu.Lock()
	lease := m.leases[key]
	if lease != nil {
		lease.Closing = true
		delete(m.leases, key)
	}
	m.mu.Unlock()
	m.closeLease(lease, "invalidated")
}

func (m *pluginRuntimeManager) InvalidatePlugin(pluginName string) {
	if m == nil || strings.TrimSpace(pluginName) == "" {
		return
	}
	m.mu.Lock()
	keys := make([]string, 0)
	for key, lease := range m.leases {
		if lease != nil && lease.Plugin == pluginName {
			keys = append(keys, key)
		}
	}
	m.mu.Unlock()
	for _, key := range keys {
		m.Invalidate(key)
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
		"state": "running", "lease_key": key, "runtime_dir": lease.RuntimeDir, "generation": lease.Generation,
		"started_at": lease.StartedAt.Format(time.RFC3339Nano), "last_used_at": lease.LastUsedAt.Format(time.RFC3339Nano),
		"holder_count": len(lease.Holders),
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
	keys := make([]string, 0, len(m.leases))
	for key := range m.leases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	leases := make([]*pluginRuntimeLease, 0, len(keys))
	for _, key := range keys {
		lease := m.leases[key]
		if lease != nil {
			lease.Closing = true
		}
		leases = append(leases, lease)
		delete(m.leases, key)
	}
	m.mu.Unlock()
	for _, lease := range leases {
		m.closeLease(lease, "runtime_close")
	}
}

func (m *pluginRuntimeManager) closeLease(lease *pluginRuntimeLease, reason string) {
	if lease == nil {
		return
	}
	lease.closeOnce.Do(func() {
		pid := 0
		if lease.Client != nil {
			pid = lease.Client.ProcessID()
		}
		logging.With("component", "plugin_runtime").Info("Plugin lease closing", "plugin", lease.Plugin, "lease_key", lease.Key, "generation", lease.Generation, "reason", reason, "pid", pid)
		if m != nil && m.lifecycle != nil {
			_ = m.lifecycle.RequestPluginShutdown(lease.Key, reason, defaultPluginShutdownGrace)
			m.lifecycle.CancelPluginPeer(lease.LifecycleToken)
		}
		if lease.Client != nil {
			lease.Client.Close()
		}
		_ = pluginstate.Remove(lease.RuntimeDir)
	})
}

const (
	pluginLeaseProbeTimeout  = 5 * time.Second
	stalePluginShutdownGrace = 2 * time.Second
)

func recoverPersistedPluginLease(runtimeDir, pluginName, scope string, ws workspace.Workspace, server config.MCPServer) error {
	status, err := pluginstate.Read(runtimeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.Plugin != pluginName || status.Runtime != config.PluginRuntimeMCP || status.Scope != scope {
		return fmt.Errorf("persisted lease identity does not match target Plugin")
	}
	if scope == config.PluginScopeWorkspace && status.WorkspaceID != ws.ID {
		return fmt.Errorf("persisted lease workspace_id %q does not match %q", status.WorkspaceID, ws.ID)
	}
	expectedArgs := server.Args
	if status.Args != nil {
		expectedArgs = status.Args
	}
	terminated, err := terminateManagedPluginProcess(status.PID, status.Executable, status.Argv0, expectedArgs, stalePluginShutdownGrace)
	if err != nil {
		return err
	}
	if terminated {
		logging.With("component", "plugin_runtime").Info("Recovered stale Plugin process", "plugin", pluginName, "pid", status.PID, "generation", status.Generation)
	}
	return pluginstate.Remove(runtimeDir)
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

func addPluginLifecycleEnv(env map[string]string, controlSocket, leaseKey, generation, token string) {
	if env == nil || strings.TrimSpace(controlSocket) == "" || strings.TrimSpace(leaseKey) == "" || strings.TrimSpace(generation) == "" || strings.TrimSpace(token) == "" {
		return
	}
	env["MCPX_CONTROL_SOCKET"] = controlSocket
	env["MCPX_PLUGIN_LEASE_KEY"] = leaseKey
	env["MCPX_PLUGIN_GENERATION"] = generation
	env["MCPX_PLUGIN_CONTROL_TOKEN"] = token
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
