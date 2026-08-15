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
	"mcpx/internal/workspace"
)

const controllerProtocolV1 = "mcpx-controller-v1"

type controllerRuntimeManager struct {
	runtime *Runtime
	mu      sync.Mutex
	leases  map[string]*controllerRuntimeLease
}

type controllerRuntimeLease struct {
	Key           string
	Plugin        string
	WorkspaceID   string
	WorkspaceName string
	WorkspacePath string
	RuntimeDir    string
	Server        config.MCPServer
	StartedAt     time.Time
	LastUsedAt    time.Time

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}

	writeMu  sync.Mutex
	stateMu  sync.Mutex
	lastErr  string
	sessions map[string]bool
	inbox    *controllerInbox
}

type controllerInbox struct {
	path   string
	mu     sync.Mutex
	next   int64
	notify chan struct{}
}

type controllerInboxRecord struct {
	Seq       int64  `json:"seq"`
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
	Event           any            `json:"event,omitempty"`
}

func newControllerRuntimeManager(runtime *Runtime) *controllerRuntimeManager {
	return &controllerRuntimeManager{runtime: runtime, leases: map[string]*controllerRuntimeLease{}}
}

func (m *controllerRuntimeManager) Ensure(ctx context.Context, pluginName string, server config.MCPServer, ws workspace.Workspace) (*controllerRuntimeLease, error) {
	if m == nil || m.runtime == nil {
		return nil, fmt.Errorf("Controller Runtime Manager is unavailable")
	}
	if server.Plugin == nil || !server.IsPlugin || server.Plugin.RuntimeType() != config.PluginRuntimeController {
		return nil, fmt.Errorf("Plugin %q does not use the Controller runtime", pluginName)
	}
	if !server.IsEnabled() {
		return nil, fmt.Errorf("Plugin %q is disabled for this Workspace", pluginName)
	}
	if strings.TrimSpace(ws.Path) == "" {
		return nil, fmt.Errorf("Controller Plugin %q requires a Workspace", pluginName)
	}
	if err := m.ensureDependencies(ctx, pluginName, server, ws); err != nil {
		return nil, err
	}
	key := pluginLeaseKey(pluginName, config.PluginScopeWorkspace, ws)

	m.mu.Lock()
	if existing := m.leases[key]; existing != nil {
		select {
		case <-existing.done:
			delete(m.leases, key)
		default:
			existing.touch()
			m.mu.Unlock()
			return existing, nil
		}
	}
	m.mu.Unlock()

	runtimeDir := m.runtime.pluginLeases.runtimeDir(pluginName, config.PluginScopeWorkspace, ws)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare Controller Plugin runtime %q: %w", runtimeDir, err)
	}
	inbox, err := openControllerInbox(filepath.Join(runtimeDir, "inbox.jsonl"))
	if err != nil {
		return nil, err
	}

	processCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(processCtx, server.Command, server.Args...)
	cmd.Dir = ws.Path
	cmd.Env = controllerProcessEnv(server, pluginRuntimeEnv(m.runtime.instanceID, m.runtime.homeDir, pluginName, config.PluginScopeWorkspace, runtimeDir, ws))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open Controller Plugin stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open Controller Plugin stdout: %w", err)
	}
	stderrPath := filepath.Join(runtimeDir, "stderr.log")
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open Controller Plugin stderr: %w", err)
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		cancel()
		return nil, fmt.Errorf("start Controller Plugin %q: %w", pluginName, err)
	}
	now := time.Now().UTC()
	lease := &controllerRuntimeLease{
		Key: key, Plugin: pluginName, WorkspaceID: ws.ID, WorkspaceName: ws.Name, WorkspacePath: ws.Path,
		RuntimeDir: runtimeDir, Server: server, StartedAt: now, LastUsedAt: now,
		cmd: cmd, stdin: stdin, cancel: cancel, done: make(chan struct{}), inbox: inbox, sessions: map[string]bool{},
	}

	m.mu.Lock()
	if duplicate := m.leases[key]; duplicate != nil {
		m.mu.Unlock()
		cancel()
		_ = stdin.Close()
		_ = cmd.Wait()
		_ = stderr.Close()
		return duplicate, nil
	}
	m.leases[key] = lease
	m.mu.Unlock()

	go m.readController(lease, stdout, stderr)
	if err := lease.send(map[string]any{
		"type": "init", "protocol": controllerProtocolV1, "plugin": pluginName,
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

func (m *controllerRuntimeManager) ensureDependencies(ctx context.Context, pluginName string, server config.MCPServer, ws workspace.Workspace) error {
	for _, depName := range server.Plugin.Depends {
		depName = strings.TrimSpace(depName)
		depServer, active, err := m.runtime.effectivePluginForWorkspace(ws.Path, depName)
		if err != nil {
			return fmt.Errorf("Controller Plugin %q dependency %q: %w", pluginName, depName, err)
		}
		if !active {
			return fmt.Errorf("Controller Plugin %q dependency %q is not enabled for Workspace %q", pluginName, depName, ws.Name)
		}
		switch depServer.Plugin.RuntimeType() {
		case config.PluginRuntimeMCP:
			prepared, err := m.runtime.prepareMCPPluginServer(ws, depName, depServer)
			if err != nil {
				return err
			}
			if _, _, err := m.runtime.pluginLeases.Ensure(ctx, depName, prepared, ws); err != nil {
				return fmt.Errorf("Controller Plugin %q dependency %q: %w", pluginName, depName, err)
			}
		case config.PluginRuntimeController:
			if _, err := m.Ensure(ctx, depName, depServer, ws); err != nil {
				return fmt.Errorf("Controller Plugin %q dependency %q: %w", pluginName, depName, err)
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

func processExitWasUnexpected(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return !strings.Contains(text, "signal: killed") && !strings.Contains(text, "context canceled")
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
	lease, tools, err := r.pluginLeases.Ensure(ctx, mount.Plugin, prepared, ws)
	if err != nil {
		return nil, err
	}
	mountDefinition := r.plugins[mount.Plugin]
	expected := mountDefinition.Tools[strings.TrimSpace(mount.Tool)]
	if expected == nil {
		return nil, fmt.Errorf("mounted tool %s/%s is not in the Global catalog", mount.Plugin, mount.Tool)
	}
	current, ok := mcpToolForLease(tools, strings.TrimSpace(mount.Tool))
	if !ok || mcpRevision([]*mcp.Tool{current}) != mcpRevision([]*mcp.Tool{expected}) {
		return nil, fmt.Errorf("mounted tool %s/%s schema changed; restart MCPX", mount.Plugin, mount.Tool)
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
			"kind": "mcpx_controller", "plugin": controller.Plugin, "workspace": controller.WorkspaceName,
			"mount": mount.Plugin + "/" + mount.Tool,
		},
	}
	if remoteSessionID != "" {
		if !controller.hasSession(remoteSessionID) {
			return nil, fmt.Errorf("remote_session_id %q is not attached to Controller Plugin %q", remoteSessionID, controller.Plugin)
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
			return fmt.Errorf("Controller mount %s/%s requires string argument %q", mount.Plugin, mount.Tool, argument)
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
	mount := r.plugins[pluginName]
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeMCP || mount.Inbox == nil {
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
	lease, tools, err := r.pluginLeases.Ensure(ctx, pluginName, prepared, ws)
	if err != nil {
		return nil, cursor, err
	}
	current, ok := mcpToolForLease(tools, mount.Inbox.Name)
	if !ok || mcpRevision([]*mcp.Tool{current}) != mcpRevision([]*mcp.Tool{mount.Inbox}) {
		return nil, cursor, fmt.Errorf("Plugin %q inbox schema changed; restart MCPX", pluginName)
	}
	arguments := map[string]any{"limit": limit, "wait_ms": waitMS}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	if err := validateDiscoveryArguments(discoverySchemaMap(current.InputSchema), arguments); err != nil {
		return nil, cursor, err
	}
	meta := mcp.Meta{mcpMetaSource: map[string]any{
		"kind": "mcpx_controller_subscription", "plugin": controller.Plugin, "source_plugin": pluginName,
		"workspace": controller.WorkspaceName,
	}}
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	if remoteSessionID != "" {
		if !controller.hasSession(remoteSessionID) {
			return nil, cursor, fmt.Errorf("remote_session_id %q is not attached to Controller Plugin %q", remoteSessionID, controller.Plugin)
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
		return fmt.Errorf("Controller Plugin %q is not running", lease.Plugin)
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
	attachedSessions := make([]string, 0, len(lease.sessions))
	for sessionID := range lease.sessions {
		attachedSessions = append(attachedSessions, sessionID)
	}
	sortStrings(attachedSessions)
	lease.stateMu.Unlock()
	state := map[string]any{
		"state": "running", "lease_key": lease.Key, "runtime_dir": lease.RuntimeDir,
		"started_at": lease.StartedAt.Format(time.RFC3339Nano), "last_used_at": lastUsedAt.Format(time.RFC3339Nano),
		"workspace_id": lease.WorkspaceID, "attached_sessions": attachedSessions,
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
	if lease == nil {
		return
	}
	lease.cancel()
	_ = lease.stdin.Close()
	select {
	case <-lease.done:
	case <-time.After(2 * time.Second):
		if lease.cmd.Process != nil {
			_ = lease.cmd.Process.Kill()
		}
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

func (inbox *controllerInbox) Append(event any) error {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	inbox.next++
	record := controllerInboxRecord{Seq: inbox.next, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Event: event}
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

func (inbox *controllerInbox) Read(cursor string, limit, waitMS int) (map[string]any, string, error) {
	sequence := int64(0)
	if strings.TrimSpace(cursor) != "" {
		parsed, err := strconv.ParseInt(strings.TrimSpace(cursor), 10, 64)
		if err != nil || parsed < 0 {
			return nil, cursor, fmt.Errorf("Controller inbox cursor must be a non-negative decimal string")
		}
		sequence = parsed
	}
	read := func() ([]controllerInboxRecord, int64, error) {
		records, err := readControllerInboxRecords(inbox.path)
		if err != nil {
			return nil, sequence, err
		}
		items := make([]controllerInboxRecord, 0, limit)
		next := sequence
		for _, record := range records {
			if record.Seq <= sequence {
				continue
			}
			items = append(items, record)
			if record.Seq > next {
				next = record.Seq
			}
			if len(items) >= limit {
				break
			}
		}
		return items, next, nil
	}
	items, next, err := read()
	if err != nil {
		return nil, cursor, err
	}
	if len(items) == 0 && waitMS > 0 {
		timer := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
		select {
		case <-inbox.notify:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		items, next, err = read()
		if err != nil {
			return nil, cursor, err
		}
	}
	nextCursor := strconv.FormatInt(next, 10)
	return map[string]any{"items": items, "next_cursor": nextCursor}, nextCursor, nil
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
