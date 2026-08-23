package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/pluginstate"
	"mcpx/internal/workspace"
)

const nativeProtocolV1 = "mcpx-native-v1"

type controllerRuntimeManager struct {
	runtime *Runtime
	mu      sync.Mutex
	leases  map[string]*controllerRuntimeLease

	startMu sync.Mutex
	starts  map[string]*controllerStartLock
}

type controllerStartLock struct {
	mu   sync.Mutex
	refs int
}

type controllerRuntimeLease struct {
	Key             string
	Plugin          string
	WorkspaceID     string
	WorkspaceName   string
	WorkspacePath   string
	RuntimeDir      string
	Server          config.MCPServer
	RuntimeRevision string
	Generation      string
	StartedAt       time.Time
	LastUsedAt      time.Time

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}

	writeMu               sync.Mutex
	stateMu               sync.Mutex
	lastErr               string
	sessions              map[string]bool
	holders               map[string]bool
	dependencyHolder      string
	dependencyReleaseOnce sync.Once
	LifecycleToken        string
	ReclaimRequested      bool
	inbox                 *controllerInbox
}

type controllerInbox struct {
	path   string
	mu     sync.Mutex
	next   int64
	notify chan struct{}
}

type controllerInboxRecord struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id,omitempty"`
	CreatedAt string `json:"created_at"`
	Event     any    `json:"event"`
}

type controllerMessage struct {
	Type            string         `json:"type"`
	ID              string         `json:"id,omitempty"`
	Mount           string         `json:"mount,omitempty"`
	Purpose         string         `json:"purpose,omitempty"`
	Arguments       map[string]any `json:"arguments,omitempty"`
	RemoteSessionID string         `json:"remote_session_id,omitempty"`
	GuidanceID      string         `json:"guidance_id,omitempty"`
	ConsumerID      string         `json:"consumer_id,omitempty"`
	IdempotencyKey  string         `json:"idempotency_key,omitempty"`
	Event           any            `json:"event,omitempty"`
}

func newControllerRuntimeManager(runtime *Runtime) *controllerRuntimeManager {
	return &controllerRuntimeManager{
		runtime: runtime,
		leases:  map[string]*controllerRuntimeLease{},
		starts:  map[string]*controllerStartLock{},
	}
}

func (m *controllerRuntimeManager) acquireStartLock(key string) func() {
	m.startMu.Lock()
	lock := m.starts[key]
	if lock == nil {
		lock = &controllerStartLock{}
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

func (m *controllerRuntimeManager) Acquire(ctx context.Context, holder, pluginName string, server config.MCPServer, ws workspace.Workspace) (*controllerRuntimeLease, error) {
	holder = strings.TrimSpace(holder)
	if holder == "" {
		return nil, fmt.Errorf("Native Plugin %q lifecycle holder is required", pluginName)
	}
	for attempt := 0; attempt < 2; attempt++ {
		lease, err := m.Ensure(ctx, pluginName, server, ws)
		if err != nil {
			return nil, err
		}
		if m.bindHolder(lease, holder) {
			return lease, nil
		}
		// The Native runtime exited after Ensure returned but before holder binding.
		// Retry once so callers never receive a detached lease with an orphan holder.
	}
	return nil, fmt.Errorf("Native Plugin %q lease changed during acquire", pluginName)
}

func (m *controllerRuntimeManager) bindHolder(lease *controllerRuntimeLease, holder string) bool {
	if m == nil || lease == nil || strings.TrimSpace(holder) == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[lease.Key] != lease {
		return false
	}
	lease.stateMu.Lock()
	defer lease.stateMu.Unlock()
	if lease.holders == nil {
		lease.holders = map[string]bool{}
	}
	lease.holders[holder] = true
	lease.ReclaimRequested = false
	lease.LastUsedAt = time.Now().UTC()
	return true
}

func (m *controllerRuntimeManager) Ensure(ctx context.Context, pluginName string, server config.MCPServer, ws workspace.Workspace) (*controllerRuntimeLease, error) {
	if m == nil || m.runtime == nil {
		return nil, fmt.Errorf("Native Runtime Manager is unavailable")
	}
	if server.Plugin == nil || !server.IsPlugin || server.Plugin.RuntimeType() != config.PluginRuntimeNative {
		return nil, fmt.Errorf("Plugin %q does not use the Native runtime", pluginName)
	}
	if !server.IsEnabled() {
		return nil, fmt.Errorf("Plugin %q is disabled for this Workspace", pluginName)
	}
	if strings.TrimSpace(ws.Path) == "" {
		return nil, fmt.Errorf("Native Plugin %q requires a Workspace", pluginName)
	}
	key := pluginLeaseKey(pluginName, config.PluginScopeWorkspace, ws)
	dependencyHolder := "native:" + key
	runtimeRevision, err := m.runtime.pluginRuntimeGraphRevision(ws.Path, pluginName, server)
	if err != nil {
		return nil, fmt.Errorf("resolve Native Plugin %q runtime graph: %w", pluginName, err)
	}

	m.mu.Lock()
	existing := m.leases[key]
	if existing != nil {
		select {
		case <-existing.done:
			delete(m.leases, key)
			existing = nil
		default:
			if existing.RuntimeRevision == runtimeRevision && existing.WorkspacePath == ws.Path {
				existing.touch()
				m.mu.Unlock()
				return existing, nil
			}
		}
	}
	m.mu.Unlock()
	if existing != nil {
		m.Invalidate(key)
	}

	// A Native runtime's dependency holder is stable for the lease key. Serialize
	// creation for that key so a failed candidate cannot roll back the same
	// boolean holder currently being established by another candidate.
	unlockStart := m.acquireStartLock(key)
	defer unlockStart()
	m.mu.Lock()
	existing = m.leases[key]
	if existing != nil {
		select {
		case <-existing.done:
			delete(m.leases, key)
			existing = nil
		default:
			if existing.RuntimeRevision == runtimeRevision && existing.WorkspacePath == ws.Path {
				existing.touch()
				m.mu.Unlock()
				return existing, nil
			}
		}
	}
	m.mu.Unlock()
	if existing != nil {
		m.Invalidate(key)
	}

	dependenciesOwned := true
	defer func() {
		if dependenciesOwned {
			m.releaseDependencyHolder(dependencyHolder)
		}
	}()
	if err := m.ensureDependencies(ctx, dependencyHolder, pluginName, server, ws); err != nil {
		return nil, err
	}
	generation, err := newLifecycleID("pg")
	if err != nil {
		return nil, fmt.Errorf("create Native Plugin %q generation: %w", pluginName, err)
	}
	controlSocket, lifecycleToken, err := m.runtime.lifecycle.PreparePluginPeer(key, pluginName, generation)
	if err != nil {
		return nil, fmt.Errorf("prepare Native Plugin %q lifecycle: %w", pluginName, err)
	}
	lifecycleOwned := lifecycleToken != ""
	defer func() {
		if lifecycleOwned && m.runtime != nil && m.runtime.lifecycle != nil {
			m.runtime.lifecycle.CancelPluginPeer(lifecycleToken)
		}
	}()

	runtimeDir := m.runtime.pluginLeases.runtimeDir(pluginName, config.PluginScopeWorkspace, ws)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare Native Plugin runtime %q: %w", runtimeDir, err)
	}
	inbox, err := openControllerInbox(filepath.Join(runtimeDir, "inbox.jsonl"))
	if err != nil {
		return nil, err
	}

	processCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(processCtx, server.Command, server.Args...)
	cmd.Dir = ws.Path
	runtimeEnv := pluginRuntimeEnv(m.runtime.instanceID, m.runtime.homeDir, pluginName, config.PluginScopeWorkspace, runtimeDir, ws)
	addPluginLifecycleEnv(runtimeEnv, controlSocket, key, generation, lifecycleToken)
	cmd.Env = controllerProcessEnv(server, runtimeEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open Native Plugin stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open Native Plugin stdout: %w", err)
	}
	stderrPath := filepath.Join(runtimeDir, "stderr.log")
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open Native Plugin stderr: %w", err)
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		cancel()
		return nil, fmt.Errorf("start Native Plugin %q: %w", pluginName, err)
	}
	now := time.Now().UTC()
	lease := &controllerRuntimeLease{
		Key: key, Plugin: pluginName, WorkspaceID: ws.ID, WorkspaceName: ws.Name, WorkspacePath: ws.Path,
		RuntimeDir: runtimeDir, Server: server, RuntimeRevision: runtimeRevision, Generation: generation, StartedAt: now, LastUsedAt: now,
		cmd: cmd, stdin: stdin, cancel: cancel, done: make(chan struct{}), inbox: inbox,
		sessions: map[string]bool{}, holders: map[string]bool{}, dependencyHolder: dependencyHolder,
		LifecycleToken: lifecycleToken,
	}

	m.mu.Lock()
	if duplicate := m.leases[key]; duplicate != nil {
		// The derived holder is keyed by this Native lease key, so the winning
		// concurrent lease owns the same dependency refs.
		dependenciesOwned = false
		m.mu.Unlock()
		cancel()
		_ = stdin.Close()
		_ = cmd.Wait()
		_ = stderr.Close()
		return duplicate, nil
	}
	if err := pluginstate.Write(runtimeDir, pluginstate.Status{
		Plugin: pluginName, Runtime: config.PluginRuntimeNative, Scope: config.PluginScopeWorkspace,
		WorkspaceID: ws.ID, WorkspaceName: ws.Name, PID: cmd.Process.Pid, Executable: cmd.Path, Argv0: cmd.Args[0],
		StartedAt: now.Format(time.RFC3339Nano), RuntimeRevision: runtimeRevision, Generation: generation,
	}); err != nil {
		m.mu.Unlock()
		cancel()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stderr.Close()
		return nil, fmt.Errorf("persist Native Plugin %q lease status: %w", pluginName, err)
	}
	m.leases[key] = lease
	dependenciesOwned = false
	lifecycleOwned = false
	m.mu.Unlock()

	go m.readController(lease, stdout, stderr)
	if err := lease.send(map[string]any{
		"type": "init", "protocol": nativeProtocolV1, "plugin": pluginName,
		"workspace":   map[string]any{"id": ws.ID, "name": ws.Name, "path": ws.Path},
		"runtime_dir": runtimeDir, "depends": server.Plugin.Depends,
		"mounts": server.Plugin.Mounts, "subscriptions": server.Plugin.Subscriptions,
	}); err != nil {
		m.Invalidate(key)
		return nil, err
	}
	for _, sub := range server.Plugin.Subscriptions {
		go m.runInboxSubscription(lease, sub)
	}
	return lease, nil
}

func (m *controllerRuntimeManager) ensureDependencies(ctx context.Context, holder, pluginName string, server config.MCPServer, ws workspace.Workspace) error {
	for _, depName := range server.Plugin.Depends {
		depName = strings.TrimSpace(depName)
		depServer, active, err := m.runtime.effectivePluginForWorkspace(ws.Path, depName)
		if err != nil {
			return fmt.Errorf("Native Plugin %q dependency %q: %w", pluginName, depName, err)
		}
		if !active {
			return fmt.Errorf("Native Plugin %q dependency %q is not enabled for Workspace %q", pluginName, depName, ws.Name)
		}
		switch depServer.Plugin.RuntimeType() {
		case config.PluginRuntimeMCP:
			prepared, err := m.runtime.prepareMCPPluginServer(ws, depName, depServer)
			if err != nil {
				return err
			}
			if _, _, err := m.runtime.pluginLeases.Acquire(ctx, holder, depName, prepared, ws); err != nil {
				return fmt.Errorf("Native Plugin %q dependency %q: %w", pluginName, depName, err)
			}
		case config.PluginRuntimeNative:
			if _, err := m.Acquire(ctx, holder, depName, depServer, ws); err != nil {
				return fmt.Errorf("Native Plugin %q dependency %q: %w", pluginName, depName, err)
			}
		}
	}
	return nil
}

func controllerProcessEnv(server config.MCPServer, runtime map[string]string) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			values[entry[:index]] = entry[index+1:]
		}
	}
	for key, value := range server.Env {
		values[key] = value
	}
	for key, value := range runtime {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (m *controllerRuntimeManager) readController(lease *controllerRuntimeLease, stdout io.Reader, stderr *os.File) {
	defer close(lease.done)
	defer stderr.Close()
	defer m.controllerExited(lease)
	defer m.removeControllerStateIfCurrent(lease)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var message controllerMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			_ = lease.inbox.Append(map[string]any{"kind": "controller_protocol_error", "error": err.Error()})
			continue
		}
		switch strings.TrimSpace(message.Type) {
		case "ready":
			// init has already pinned the workspace/mount contract.
		case "emit":
			if message.Event != nil {
				_ = lease.inbox.Append(message.Event)
			}
		case "call":
			go m.handleControllerCall(lease, message)
		case "guidance_bind":
			go m.handleControllerGuidanceBind(lease, message)
		default:
			_ = lease.inbox.Append(map[string]any{"kind": "controller_protocol_error", "error": "unsupported message type", "type": message.Type})
		}
	}
	if err := scanner.Err(); err != nil {
		lease.setError(err.Error())
	}
	if err := lease.cmd.Wait(); err != nil && processExitWasUnexpected(err) {
		lease.setError(err.Error())
		_ = lease.inbox.Append(map[string]any{"kind": "controller_unavailable", "error": err.Error()})
	}
}

func (m *controllerRuntimeManager) removeControllerStateIfCurrent(lease *controllerRuntimeLease) {
	if m == nil || lease == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[lease.Key] != lease {
		return
	}
	_ = pluginstate.Remove(lease.RuntimeDir)
}

func processExitWasUnexpected(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return !strings.Contains(text, "signal: killed") && !strings.Contains(text, "context canceled")
}

func (m *controllerRuntimeManager) handleControllerGuidanceBind(lease *controllerRuntimeLease, message controllerMessage) {
	id := strings.TrimSpace(message.ID)
	remoteSessionID := strings.TrimSpace(message.RemoteSessionID)
	guidanceID := strings.TrimSpace(message.GuidanceID)
	consumerID := strings.TrimSpace(message.ConsumerID)
	deliveryKey := strings.TrimSpace(message.IdempotencyKey)
	purpose := strings.TrimSpace(message.Purpose)
	if id == "" {
		_ = lease.inbox.Append(map[string]any{"kind": "controller_protocol_error", "error": "guidance_bind requires id"})
		return
	}
	if remoteSessionID == "" || guidanceID == "" || consumerID == "" || deliveryKey == "" || purpose == "" {
		_ = lease.send(map[string]any{"type": "result", "id": id, "ok": false, "error": "guidance_bind requires remote_session_id, guidance_id, consumer_id, purpose and idempotency_key"})
		return
	}
	if !lease.hasSession(remoteSessionID) {
		_ = lease.send(map[string]any{"type": "result", "id": id, "ok": false, "error": "Remote Session is not attached to this Controller"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := m.runtime.bindContextGuidance(ctx, remoteSessionID, lease.WorkspacePath, guidanceID, consumerID, deliveryKey, lease.Plugin)
	if err != nil {
		_ = lease.send(map[string]any{"type": "result", "id": id, "ok": false, "error": err.Error()})
		return
	}
	_ = lease.send(map[string]any{"type": "result", "id": id, "ok": true, "result": result})
}

func (m *controllerRuntimeManager) handleControllerCall(lease *controllerRuntimeLease, message controllerMessage) {
	id := strings.TrimSpace(message.ID)
	if id == "" {
		_ = lease.inbox.Append(map[string]any{"kind": "controller_protocol_error", "error": "call requires id"})
		return
	}
	mount, ok := lease.Server.Plugin.Mounts[strings.TrimSpace(message.Mount)]
	if !ok {
		_ = lease.send(map[string]any{"type": "result", "id": id, "ok": false, "error": "unknown mount"})
		return
	}
	if !mount.Automatic {
		_ = lease.send(map[string]any{"type": "result", "id": id, "ok": false, "error": "mount is owner-gated; emit an inbox request instead"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := m.runtime.callControllerMount(ctx, lease, mount, message.Arguments, strings.TrimSpace(message.RemoteSessionID))
	if err != nil {
		_ = lease.send(map[string]any{"type": "result", "id": id, "ok": false, "error": err.Error()})
		return
	}
	_ = lease.send(map[string]any{"type": "result", "id": id, "ok": true, "result": result})
}

func (r *Runtime) callControllerMount(ctx context.Context, controller *controllerRuntimeLease, mount config.MCPPluginMount, arguments map[string]any, remoteSessionID string) (map[string]any, error) {
	server, active, err := r.effectivePluginForWorkspace(controller.WorkspacePath, mount.Plugin)
	if err != nil {
		return nil, err
	}
	if !active || server.Plugin == nil || server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return nil, fmt.Errorf("mounted Plugin %q is unavailable", mount.Plugin)
	}
	ws := r.workspaceRuntime(controller.WorkspaceName, controller.WorkspacePath)
	prepared, err := r.prepareMCPPluginServer(ws, mount.Plugin, server)
	if err != nil {
		return nil, err
	}
	lease, tools, err := r.pluginLeases.Acquire(ctx, controller.dependencyHolder, mount.Plugin, prepared, ws)
	if err != nil {
		return nil, err
	}
	mountDefinition, ok, err := r.pluginMountForWorkspace(controller.WorkspacePath, mount.Plugin)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("mounted Plugin %q definition is unavailable", mount.Plugin)
	}
	toolName := strings.TrimSpace(mount.Tool)
	if !pluginAllowsTool(mountDefinition.Server.Plugin, toolName) {
		return nil, fmt.Errorf("mounted tool %s/%s is not in the effective plugin.tools allowlist", mount.Plugin, mount.Tool)
	}
	current, ok := mcpToolForLease(tools, toolName)
	if !ok {
		return nil, fmt.Errorf("mounted tool %s/%s is not returned by the current Plugin runtime", mount.Plugin, mount.Tool)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if err := enforceControllerMountGuards(mount, arguments); err != nil {
		return nil, err
	}
	if err := validateDiscoveryArguments(discoverySchemaMap(current.InputSchema), arguments); err != nil {
		return nil, err
	}
	meta := mcp.Meta{
		mcpMetaSource: map[string]any{
			"kind": "mcpx_native_use", "plugin": controller.Plugin, "workspace": controller.WorkspaceName,
			"mount": mount.Plugin + "/" + mount.Tool,
		},
	}
	if remoteSessionID != "" {
		if !controller.hasSession(remoteSessionID) {
			return nil, fmt.Errorf("remote_session_id %q is not attached to Native Plugin %q", remoteSessionID, controller.Plugin)
		}
		meta[mcpMetaRemoteSessionID] = remoteSessionID
	}
	result, err := lease.Client.CallTool(ctx, current.Name, arguments, meta)
	if err != nil {
		return nil, err
	}
	return controllerToolResult(result), nil
}

func enforceControllerMountGuards(mount config.MCPPluginMount, arguments map[string]any) error {
	for argument, guard := range mount.Guards {
		value, ok := arguments[argument].(string)
		if !ok {
			return fmt.Errorf("Native use %s/%s requires string argument %q", mount.Plugin, mount.Tool, argument)
		}
		switch {
		case guard.Equals != "":
			if value != guard.Equals {
				return fmt.Errorf("Controller mount %s/%s argument %q must equal %q", mount.Plugin, mount.Tool, argument, guard.Equals)
			}
		case guard.Prefix != "":
			if !strings.HasPrefix(value, guard.Prefix) {
				return fmt.Errorf("Controller mount %s/%s argument %q must start with %q", mount.Plugin, mount.Tool, argument, guard.Prefix)
			}
		case len(guard.OneOf) > 0:
			allowed := false
			for _, candidate := range guard.OneOf {
				if value == candidate {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("Controller mount %s/%s argument %q is outside the allowed set", mount.Plugin, mount.Tool, argument)
			}
		}
	}
	return nil
}

func controllerToolResult(result *mcp.CallToolResult) map[string]any {
	if result == nil {
		return map[string]any{"is_error": true, "error": "Plugin returned no result"}
	}
	return map[string]any{
		"content": result.Content, "structured_content": result.StructuredContent, "is_error": result.IsError,
	}
}

func (m *controllerRuntimeManager) runInboxSubscription(lease *controllerRuntimeLease, subscription config.MCPPluginSubscription) {
	scope := strings.TrimSpace(subscription.Scope)
	if scope == "" {
		scope = config.PluginSubscriptionScopeWorkspace
	}
	if scope == config.PluginSubscriptionScopeSessions {
		m.runSessionInboxSubscription(lease, subscription)
		return
	}
	m.runWorkspaceInboxSubscription(lease, subscription)
}

func (m *controllerRuntimeManager) runWorkspaceInboxSubscription(lease *controllerRuntimeLease, subscription config.MCPPluginSubscription) {
	cursor := ""
	for {
		select {
		case <-lease.done:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		result, next, err := m.runtime.controllerReadDependencyInbox(ctx, lease, strings.TrimSpace(subscription.Plugin), cursor, 100, 25000, "")
		cancel()
		if err != nil {
			_ = lease.send(map[string]any{"type": "event", "source": map[string]any{"plugin": subscription.Plugin, "kind": "availability"}, "event": map[string]any{"state": "unavailable", "error": err.Error()}})
			select {
			case <-lease.done:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if next != "" {
			cursor = next
		}
		if controllerInboxResultHasItems(result) {
			if err := lease.send(map[string]any{"type": "event", "source": map[string]any{"plugin": subscription.Plugin, "kind": config.PluginSubscriptionInbox}, "event": result}); err != nil {
				return
			}
			continue
		}
		select {
		case <-lease.done:
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type controllerSessionInboxResult struct {
	remoteSessionID string
	result          map[string]any
	next            string
	err             error
}

func (m *controllerRuntimeManager) runSessionInboxSubscription(lease *controllerRuntimeLease, subscription config.MCPPluginSubscription) {
	cursors := map[string]string{}
	for {
		select {
		case <-lease.done:
			return
		default:
		}
		sessions := lease.sessionIDs()
		if len(sessions) == 0 {
			select {
			case <-lease.done:
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		results := make(chan controllerSessionInboxResult, len(sessions))
		for _, remoteSessionID := range sessions {
			remoteSessionID := remoteSessionID
			cursor := cursors[remoteSessionID]
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
				defer cancel()
				result, next, err := m.runtime.controllerReadDependencyInbox(ctx, lease, strings.TrimSpace(subscription.Plugin), cursor, 100, 25000, remoteSessionID)
				results <- controllerSessionInboxResult{remoteSessionID: remoteSessionID, result: result, next: next, err: err}
			}()
		}
		hadError := false
		for range sessions {
			outcome := <-results
			if !lease.hasSession(outcome.remoteSessionID) {
				delete(cursors, outcome.remoteSessionID)
				continue
			}
			if outcome.err != nil {
				hadError = true
				_ = lease.send(map[string]any{
					"type":   "event",
					"source": map[string]any{"plugin": subscription.Plugin, "kind": "availability", "remote_session_id": outcome.remoteSessionID},
					"event":  map[string]any{"state": "unavailable", "error": outcome.err.Error()},
				})
				continue
			}
			if outcome.next != "" {
				cursors[outcome.remoteSessionID] = outcome.next
			}
			if !controllerInboxResultHasItems(outcome.result) {
				continue
			}
			if err := lease.send(map[string]any{
				"type":   "event",
				"source": map[string]any{"plugin": subscription.Plugin, "kind": config.PluginSubscriptionInbox, "remote_session_id": outcome.remoteSessionID},
				"event":  outcome.result,
			}); err != nil {
				return
			}
		}
		for sessionID := range cursors {
			if !lease.hasSession(sessionID) {
				delete(cursors, sessionID)
			}
		}
		backoff := 100 * time.Millisecond
		if hadError {
			backoff = 2 * time.Second
		}
		select {
		case <-lease.done:
			return
		case <-time.After(backoff):
		}
	}
}

func controllerInboxResultHasItems(result map[string]any) bool {
	if result == nil {
		return false
	}
	structured, _ := result["structured_content"].(map[string]any)
	if structured == nil {
		return false
	}
	switch items := structured["items"].(type) {
	case []any:
		return len(items) > 0
	case []map[string]any:
		return len(items) > 0
	default:
		return false
	}
}

func (r *Runtime) controllerReadDependencyInbox(ctx context.Context, controller *controllerRuntimeLease, pluginName, cursor string, limit, waitMS int, remoteSessionID string) (map[string]any, string, error) {
	mount, ok, err := r.pluginMountForWorkspace(controller.WorkspacePath, pluginName)
	if err != nil {
		return nil, cursor, err
	}
	if !ok {
		return nil, cursor, fmt.Errorf("Plugin %q definition is unavailable", pluginName)
	}
	inboxName := ""
	if mount.Server.Plugin != nil {
		inboxName = strings.TrimSpace(mount.Server.Plugin.Inbox)
	}
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeMCP || inboxName == "" {
		return nil, cursor, fmt.Errorf("Plugin %q has no MCP inbox", pluginName)
	}
	server, active, err := r.effectivePluginForWorkspace(controller.WorkspacePath, pluginName)
	if err != nil {
		return nil, cursor, err
	}
	if !active {
		return nil, cursor, fmt.Errorf("Plugin %q is disabled", pluginName)
	}
	ws := r.workspaceRuntime(controller.WorkspaceName, controller.WorkspacePath)
	prepared, err := r.prepareMCPPluginServer(ws, pluginName, server)
	if err != nil {
		return nil, cursor, err
	}
	lease, tools, err := r.pluginLeases.Acquire(ctx, controller.dependencyHolder, pluginName, prepared, ws)
	if err != nil {
		return nil, cursor, err
	}
	current, ok := mcpToolForLease(tools, inboxName)
	if !ok {
		return nil, cursor, fmt.Errorf("Plugin %q current runtime did not return inbox tool %q", pluginName, inboxName)
	}
	arguments := map[string]any{"limit": limit, "wait_ms": waitMS}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	if err := validateDiscoveryArguments(discoverySchemaMap(current.InputSchema), arguments); err != nil {
		return nil, cursor, err
	}
	meta := mcp.Meta{mcpMetaSource: map[string]any{
		"kind": "mcpx_native_watch", "plugin": controller.Plugin, "source_plugin": pluginName,
		"workspace": controller.WorkspaceName,
	}}
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	if remoteSessionID != "" {
		if !controller.hasSession(remoteSessionID) {
			return nil, cursor, fmt.Errorf("remote_session_id %q is not attached to Native Plugin %q", remoteSessionID, controller.Plugin)
		}
		meta[mcpMetaRemoteSessionID] = remoteSessionID
	}
	result, err := lease.Client.CallTool(ctx, current.Name, arguments, meta)
	if err != nil {
		return nil, cursor, err
	}
	if result == nil {
		return nil, cursor, fmt.Errorf("Plugin %q inbox returned no result", pluginName)
	}
	next := inboxResultCursor(result)
	return controllerToolResult(result), next, nil
}

func (lease *controllerRuntimeLease) send(message any) error {
	lease.writeMu.Lock()
	defer lease.writeMu.Unlock()
	select {
	case <-lease.done:
		return fmt.Errorf("Native Plugin %q is not running", lease.Plugin)
	default:
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = lease.stdin.Write(encoded)
	return err
}

func (lease *controllerRuntimeLease) setError(message string) {
	lease.stateMu.Lock()
	lease.lastErr = message
	lease.stateMu.Unlock()
}

func (lease *controllerRuntimeLease) touch() {
	lease.stateMu.Lock()
	lease.ReclaimRequested = false
	lease.LastUsedAt = time.Now().UTC()
	lease.stateMu.Unlock()
}

func (lease *controllerRuntimeLease) attachSession(remoteSessionID string) bool {
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	if remoteSessionID == "" {
		return false
	}
	lease.stateMu.Lock()
	defer lease.stateMu.Unlock()
	if lease.sessions[remoteSessionID] {
		return false
	}
	lease.sessions[remoteSessionID] = true
	return true
}

func (lease *controllerRuntimeLease) detachSession(remoteSessionID string) bool {
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	lease.stateMu.Lock()
	defer lease.stateMu.Unlock()
	if !lease.sessions[remoteSessionID] {
		return false
	}
	delete(lease.sessions, remoteSessionID)
	return true
}

func (lease *controllerRuntimeLease) hasSession(remoteSessionID string) bool {
	lease.stateMu.Lock()
	defer lease.stateMu.Unlock()
	return lease.sessions[strings.TrimSpace(remoteSessionID)]
}

func (lease *controllerRuntimeLease) sessionIDs() []string {
	lease.stateMu.Lock()
	defer lease.stateMu.Unlock()
	out := make([]string, 0, len(lease.sessions))
	for sessionID := range lease.sessions {
		out = append(out, sessionID)
	}
	sortStrings(out)
	return out
}

func (lease *controllerRuntimeLease) state() map[string]any {
	lease.stateMu.Lock()
	lastErr := lease.lastErr
	lastUsedAt := lease.LastUsedAt
	holderCount := len(lease.holders)
	attachedSessions := make([]string, 0, len(lease.sessions))
	for sessionID := range lease.sessions {
		attachedSessions = append(attachedSessions, sessionID)
	}
	sortStrings(attachedSessions)
	lease.stateMu.Unlock()
	state := map[string]any{
		"state": "running", "lease_key": lease.Key, "runtime_dir": lease.RuntimeDir, "generation": lease.Generation,
		"started_at": lease.StartedAt.Format(time.RFC3339Nano), "last_used_at": lastUsedAt.Format(time.RFC3339Nano),
		"workspace_id": lease.WorkspaceID, "attached_sessions": attachedSessions, "holder_count": holderCount,
	}
	select {
	case <-lease.done:
		state["state"] = "stopped"
	default:
	}
	if lastErr != "" {
		state["error"] = lastErr
	}
	return state
}

func (m *controllerRuntimeManager) AttachSession(remoteSessionID, workspaceID, workspaceName string) {
	if m == nil || strings.TrimSpace(remoteSessionID) == "" || strings.TrimSpace(workspaceID) == "" {
		return
	}
	m.mu.Lock()
	leases := make([]*controllerRuntimeLease, 0, len(m.leases))
	for _, lease := range m.leases {
		if lease.WorkspaceID == workspaceID {
			leases = append(leases, lease)
		}
	}
	m.mu.Unlock()
	for _, lease := range leases {
		if lease.attachSession(remoteSessionID) {
			_ = lease.send(map[string]any{
				"type": "event", "source": map[string]any{"kind": "session.opened"},
				"event": map[string]any{"remote_session_id": remoteSessionID, "workspace": workspaceName},
			})
		}
	}
}

func (m *controllerRuntimeManager) DetachSession(remoteSessionID string) {
	if m == nil || strings.TrimSpace(remoteSessionID) == "" {
		return
	}
	m.mu.Lock()
	leases := make([]*controllerRuntimeLease, 0, len(m.leases))
	for _, lease := range m.leases {
		leases = append(leases, lease)
	}
	m.mu.Unlock()
	for _, lease := range leases {
		if lease.detachSession(remoteSessionID) {
			_ = lease.send(map[string]any{
				"type": "event", "source": map[string]any{"kind": "session.closed"},
				"event": map[string]any{"remote_session_id": remoteSessionID},
			})
		}
	}
	m.ReleaseHolder(lifecycleSessionHolderID(remoteSessionID))
}

func (m *controllerRuntimeManager) ReleaseHolder(holder string) int {
	if m == nil || strings.TrimSpace(holder) == "" {
		return 0
	}
	holder = strings.TrimSpace(holder)
	m.mu.Lock()
	keys := make([]string, 0)
	for key, lease := range m.leases {
		if lease == nil {
			continue
		}
		lease.stateMu.Lock()
		had := lease.holders[holder]
		if had {
			delete(lease.holders, holder)
		}
		empty := had && (len(lease.holders) == 0 || (lease.ReclaimRequested && !hasBlockingPluginHolders(lease.holders)))
		lease.stateMu.Unlock()
		if empty {
			keys = append(keys, key)
		}
	}
	m.mu.Unlock()
	for _, key := range keys {
		m.Invalidate(key)
	}
	return len(keys)
}

func (m *controllerRuntimeManager) State(pluginName string, ws workspace.Workspace) map[string]any {
	if m == nil {
		return map[string]any{"state": "unavailable"}
	}
	key := pluginLeaseKey(pluginName, config.PluginScopeWorkspace, ws)
	m.mu.Lock()
	lease := m.leases[key]
	m.mu.Unlock()
	if lease == nil {
		return map[string]any{"state": "configured", "lease_key": key}
	}
	return lease.state()
}

func (m *controllerRuntimeManager) Invalidate(key string) {
	if m == nil || strings.TrimSpace(key) == "" {
		return
	}
	m.mu.Lock()
	lease := m.leases[key]
	delete(m.leases, key)
	m.mu.Unlock()
	m.closeControllerLease(lease, "invalidated")
}

func (m *controllerRuntimeManager) closeControllerLease(lease *controllerRuntimeLease, reason string) {
	if m == nil || lease == nil {
		return
	}
	if m.runtime != nil && m.runtime.lifecycle != nil {
		_ = m.runtime.lifecycle.RequestPluginShutdown(lease.Key, reason, defaultPluginShutdownGrace)
		m.runtime.lifecycle.CancelPluginPeer(lease.LifecycleToken)
	}
	lease.cancel()
	_ = lease.stdin.Close()
	_ = pluginstate.Remove(lease.RuntimeDir)
	select {
	case <-lease.done:
	case <-time.After(2 * time.Second):
		if lease.cmd.Process != nil {
			_ = lease.cmd.Process.Kill()
		}
	}
	m.releaseControllerDependencies(lease)
}

func (m *controllerRuntimeManager) controllerExited(lease *controllerRuntimeLease) {
	if m == nil || lease == nil {
		return
	}
	m.mu.Lock()
	if m.leases[lease.Key] == lease {
		delete(m.leases, lease.Key)
	}
	m.mu.Unlock()
	if m.runtime != nil && m.runtime.lifecycle != nil {
		m.runtime.lifecycle.CancelPluginPeer(lease.LifecycleToken)
	}
	m.releaseControllerDependencies(lease)
}

func (m *controllerRuntimeManager) releaseDependencyHolder(holder string) {
	if m == nil || strings.TrimSpace(holder) == "" {
		return
	}
	if m.runtime != nil && m.runtime.pluginLeases != nil {
		m.runtime.pluginLeases.ReleaseHolder(holder)
	}
	// A Controller may itself depend on another Controller. Release this
	// derived holder after MCP dependencies, outside all manager locks.
	m.ReleaseHolder(holder)
}

func (m *controllerRuntimeManager) releaseControllerDependencies(lease *controllerRuntimeLease) {
	if m == nil || lease == nil {
		return
	}
	lease.dependencyReleaseOnce.Do(func() {
		m.releaseDependencyHolder(lease.dependencyHolder)
	})
}

func (m *controllerRuntimeManager) InvalidatePlugin(pluginName string) {
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

func (m *controllerRuntimeManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	keys := make([]string, 0, len(m.leases))
	for key := range m.leases {
		keys = append(keys, key)
	}
	m.mu.Unlock()
	for _, key := range keys {
		m.Invalidate(key)
	}
}

func openControllerInbox(path string) (*controllerInbox, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	inbox := &controllerInbox{path: path, notify: make(chan struct{}, 1)}
	records, err := readControllerInboxRecords(path)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Seq > inbox.next {
			inbox.next = record.Seq
		}
	}
	return inbox, nil
}

func normalizeNativeInboxEvent(event any) (map[string]any, bool, error) {
	value, ok := event.(map[string]any)
	if !ok || value == nil {
		return nil, false, fmt.Errorf("Native Inbox event must be an object")
	}
	kind, _ := value["kind"].(string)
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil, false, fmt.Errorf("Native Inbox event requires kind")
	}
	actionRequired, _ := value["action_required"].(bool)
	delivery, _ := value["delivery"].(string)
	delivery = strings.TrimSpace(delivery)
	if delivery == "drop" {
		if actionRequired {
			return nil, false, fmt.Errorf("Native Inbox action_required event cannot use delivery=drop")
		}
		return nil, false, nil
	}
	if delivery == "" {
		// Compatibility for older Native Plugins during V1 migration. Bundled V1
		// Plugins emit delivery explicitly; unknown no-action events are deferred.
		if actionRequired {
			delivery = "immediate"
		} else {
			delivery = "deferred"
		}
	}
	if delivery != "immediate" && delivery != "deferred" {
		return nil, false, fmt.Errorf("Native Inbox event %q delivery must be immediate or deferred", kind)
	}
	if actionRequired && delivery != "immediate" {
		return nil, false, fmt.Errorf("Native Inbox event %q action_required=true requires delivery=immediate", kind)
	}
	summary, _ := value["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		summary = kind
	}
	normalized := make(map[string]any, len(value)+4)
	for key, item := range value {
		normalized[key] = item
	}
	normalized["kind"] = kind
	normalized["delivery"] = delivery
	normalized["action_required"] = actionRequired
	normalized["summary"] = summary
	return normalized, true, nil
}

func nativeInboxRecordItem(record controllerInboxRecord) (map[string]any, bool, error) {
	event, admitted, err := normalizeNativeInboxEvent(record.Event)
	if err != nil || !admitted {
		return nil, admitted, err
	}
	item := make(map[string]any, len(event)+3)
	for key, value := range event {
		item[key] = value
	}
	item["seq"] = record.Seq
	if strings.TrimSpace(record.ID) != "" {
		item["id"] = record.ID
	} else {
		item["id"] = fmt.Sprintf("native_inbox_%d", record.Seq)
	}
	item["created_at"] = record.CreatedAt
	return item, true, nil
}

func nativeInboxHasImmediate(items []map[string]any) bool {
	for _, item := range items {
		if delivery, _ := item["delivery"].(string); delivery == "immediate" {
			return true
		}
	}
	return false
}

func (inbox *controllerInbox) Append(event any) error {
	normalized, admitted, err := normalizeNativeInboxEvent(event)
	if err != nil {
		return err
	}
	if !admitted {
		return nil
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	inbox.next++
	record := controllerInboxRecord{
		Seq: inbox.next, ID: fmt.Sprintf("native_inbox_%d", inbox.next),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Event: normalized,
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(inbox.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(body, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	select {
	case inbox.notify <- struct{}{}:
	default:
	}
	return nil
}

func (inbox *controllerInbox) Read(ctx context.Context, cursor string, limit, waitMS int) (map[string]any, string, error) {
	sequence := int64(0)
	if strings.TrimSpace(cursor) != "" {
		parsed, err := strconv.ParseInt(strings.TrimSpace(cursor), 10, 64)
		if err != nil || parsed < 0 {
			return nil, cursor, fmt.Errorf("Native Inbox cursor must be a non-negative decimal string")
		}
		sequence = parsed
	}
	if limit < 1 {
		limit = 50
	}
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > 60000 {
		waitMS = 60000
	}
	read := func() ([]map[string]any, int64, bool, error) {
		records, err := readControllerInboxRecords(inbox.path)
		if err != nil {
			return nil, sequence, false, err
		}
		matching := make([]map[string]any, 0)
		for _, record := range records {
			if record.Seq <= sequence {
				continue
			}
			item, admitted, itemErr := nativeInboxRecordItem(record)
			if itemErr != nil {
				return nil, sequence, false, itemErr
			}
			if admitted {
				matching = append(matching, item)
			}
		}
		hasMore := len(matching) > limit
		if hasMore {
			matching = matching[:limit]
		}
		next := sequence
		if len(matching) > 0 {
			if seq, ok := matching[len(matching)-1]["seq"].(int64); ok {
				next = seq
			}
		}
		return matching, next, hasMore, nil
	}
	items, next, hasMore, err := read()
	if err != nil {
		return nil, cursor, err
	}
	result := func(timedOut bool) (map[string]any, string) {
		nextCursor := strconv.FormatInt(next, 10)
		return map[string]any{
			"schema_version": 3, "items": items, "has_more": hasMore,
			"timed_out": timedOut, "next_cursor": nextCursor,
		}, nextCursor
	}
	if waitMS == 0 || hasMore || nativeInboxHasImmediate(items) {
		value, nextCursor := result(false)
		return value, nextCursor, nil
	}

	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			value, nextCursor := result(true)
			return value, nextCursor, nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, cursor, ctx.Err()
		case <-inbox.notify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		items, next, hasMore, err = read()
		if err != nil {
			return nil, cursor, err
		}
		if hasMore || nativeInboxHasImmediate(items) {
			value, nextCursor := result(false)
			return value, nextCursor, nil
		}
		if time.Now().After(deadline) || time.Now().Equal(deadline) {
			value, nextCursor := result(true)
			return value, nextCursor, nil
		}
		// Deferred items remain in the same cursor window while we continue
		// waiting for an immediate event or the configured deadline.
	}
}

func readControllerInboxRecords(path string) ([]controllerInboxRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	records := []controllerInboxRecord{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record controllerInboxRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}
