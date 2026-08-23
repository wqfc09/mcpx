package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
)

const (
	defaultLifecycleStartupGrace = 30 * time.Second
	defaultLifecycleIdleGrace    = 3 * time.Second
)

type lifecycleRoot struct {
	ID            string
	Kind          string
	WorkspaceID   string
	WorkspaceName string
}

type lifecycleManager struct {
	runtime      *Runtime
	startupGrace time.Duration
	idleGrace    time.Duration

	mu                  sync.Mutex
	roots               map[string]lifecycleRoot
	listener            net.Listener
	socketPath          string
	idleTimer           *time.Timer
	generation          uint64
	closing             bool
	connections         map[net.Conn]bool
	pluginRegistrations map[string]lifecyclePluginRegistration
	pluginPeers         map[string]*lifecyclePluginPeer

	sessionGateMu sync.Mutex
	sessionGates  map[string]*lifecycleSessionGate
}

type lifecycleSessionGate struct {
	mu   sync.Mutex
	refs int
}

type lifecycleRequestTrackerKey struct{}

type lifecycleRequestTracker struct {
	manager *lifecycleManager
	mu      sync.Mutex
	roots   []string
}

type lifecycleControlRequest struct {
	Type          string `json:"type"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	Plugin        string `json:"plugin,omitempty"`
	LeaseKey      string `json:"lease_key,omitempty"`
	Generation    string `json:"generation,omitempty"`
	Token         string `json:"token,omitempty"`
}

func newLifecycleManager(runtime *Runtime, startupGrace, idleGrace time.Duration) *lifecycleManager {
	if startupGrace <= 0 {
		startupGrace = defaultLifecycleStartupGrace
	}
	if idleGrace <= 0 {
		idleGrace = defaultLifecycleIdleGrace
	}
	return &lifecycleManager{
		runtime: runtime, startupGrace: startupGrace, idleGrace: idleGrace,
		roots: map[string]lifecycleRoot{}, connections: map[net.Conn]bool{},
		pluginRegistrations: map[string]lifecyclePluginRegistration{}, pluginPeers: map[string]*lifecyclePluginPeer{},
		sessionGates: map[string]*lifecycleSessionGate{},
	}
}

func (m *lifecycleManager) Start() error {
	if m == nil || m.runtime == nil {
		return nil
	}
	path, err := runtimeinstance.ControlSocketPath()
	if err != nil {
		return err
	}
	listener, err := listenLifecycleControlSocket(path)
	if err != nil {
		return err
	}
	// Windows V1 intentionally has no local lifecycle transport yet. Do not arm
	// idle shutdown when no client can acquire a root holder.
	if listener == nil {
		return nil
	}
	m.mu.Lock()
	m.listener = listener
	m.socketPath = path
	m.scheduleIdleLocked(m.startupGrace)
	m.mu.Unlock()
	go m.acceptLoop(listener)
	return nil
}

func (m *lifecycleManager) SocketPath() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.socketPath
}

func (m *lifecycleManager) RootCount() int {
	count, _, _ := m.instanceStatus()
	return count
}

func (m *lifecycleManager) instanceStatus() (rootHolders, workbenchHolders int, closing bool) {
	if m == nil {
		return 0, 0, true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, root := range m.roots {
		if root.Kind == "workbench" {
			workbenchHolders++
		}
	}
	return len(m.roots), workbenchHolders, m.closing
}

func (m *lifecycleManager) acquireRoot(root lifecycleRoot) error {
	if m == nil {
		return errors.New("lifecycle manager is unavailable")
	}
	root.ID = strings.TrimSpace(root.ID)
	if root.ID == "" {
		return errors.New("root holder id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errors.New("MCPX lifecycle is shutting down")
	}
	m.roots[root.ID] = root
	m.cancelIdleLocked()
	return nil
}

func (m *lifecycleManager) releaseRoot(id string) (remaining int, willShutdown bool) {
	if m == nil {
		return 0, false
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.roots[id]; !exists {
		return len(m.roots), false
	}
	delete(m.roots, id)
	remaining = len(m.roots)
	if remaining == 0 && !m.closing && m.listener != nil {
		m.scheduleIdleLocked(m.idleGrace)
		willShutdown = true
	}
	return remaining, willShutdown
}

func lifecycleSessionHolderID(remoteSessionID string) string {
	return "session:" + strings.TrimSpace(remoteSessionID)
}

func (m *lifecycleManager) lockSession(remoteSessionID string) func() {
	if m == nil {
		return func() {}
	}
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	m.sessionGateMu.Lock()
	gate := m.sessionGates[remoteSessionID]
	if gate == nil {
		gate = &lifecycleSessionGate{}
		m.sessionGates[remoteSessionID] = gate
	}
	gate.refs++
	m.sessionGateMu.Unlock()
	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		m.sessionGateMu.Lock()
		gate.refs--
		if gate.refs == 0 && m.sessionGates[remoteSessionID] == gate {
			delete(m.sessionGates, remoteSessionID)
		}
		m.sessionGateMu.Unlock()
	}
}

func (m *lifecycleManager) acquireSessionUsage(remoteSessionID, workspaceID, workspaceName string, withRequest bool) (string, error) {
	if m == nil {
		return "", errors.New("lifecycle manager is unavailable")
	}
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	if remoteSessionID == "" {
		return "", errors.New("remote_session_id is required for lifecycle request")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return "", errors.New("MCPX lifecycle is shutting down")
	}
	// Remote Session identity is durable state, not process ownership. It must
	// survive MCPX restarts without keeping the Instance resident. Only an
	// in-flight request for that Session temporarily holds the Instance alive.
	if !withRequest {
		return "", nil
	}
	id, err := newLifecycleID("req")
	if err != nil {
		return "", err
	}
	requestRootID := "request:" + remoteSessionID + ":" + id
	m.roots[requestRootID] = lifecycleRoot{
		ID: requestRootID, Kind: "request",
		WorkspaceID: strings.TrimSpace(workspaceID), WorkspaceName: strings.TrimSpace(workspaceName),
	}
	m.cancelIdleLocked()
	return requestRootID, nil
}

func (m *lifecycleManager) AcquireSession(remoteSessionID, workspaceID, workspaceName string) error {
	_, err := m.acquireSessionUsage(remoteSessionID, workspaceID, workspaceName, false)
	return err
}

func (m *lifecycleManager) ReleaseSession(remoteSessionID string) (remaining int, willShutdown bool) {
	return m.RootCount(), false
}

func withLifecycleRequestTracker(ctx context.Context, manager *lifecycleManager) (context.Context, *lifecycleRequestTracker) {
	if manager == nil {
		return ctx, nil
	}
	tracker := &lifecycleRequestTracker{manager: manager}
	return context.WithValue(ctx, lifecycleRequestTrackerKey{}, tracker), tracker
}

func lifecycleRequestTrackerFrom(ctx context.Context) *lifecycleRequestTracker {
	tracker, _ := ctx.Value(lifecycleRequestTrackerKey{}).(*lifecycleRequestTracker)
	return tracker
}

func (t *lifecycleRequestTracker) track(rootID string) {
	if t == nil || strings.TrimSpace(rootID) == "" {
		return
	}
	t.mu.Lock()
	t.roots = append(t.roots, rootID)
	t.mu.Unlock()
}

func (t *lifecycleRequestTracker) releaseAll() {
	if t == nil || t.manager == nil {
		return
	}
	t.mu.Lock()
	roots := append([]string(nil), t.roots...)
	t.roots = nil
	t.mu.Unlock()
	for _, rootID := range roots {
		t.manager.releaseRoot(rootID)
	}
}

func lifecycleOperationHolderID(operationID string) string {
	return "operation:" + strings.TrimSpace(operationID)
}

func (m *lifecycleManager) AcquireOperation(operationID, workspaceName string) error {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return errors.New("operation_id is required for lifecycle root")
	}
	return m.acquireRoot(lifecycleRoot{
		ID: lifecycleOperationHolderID(operationID), Kind: "operation", WorkspaceName: strings.TrimSpace(workspaceName),
	})
}

func (m *lifecycleManager) ReleaseOperation(operationID string) (remaining int, willShutdown bool) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return m.RootCount(), false
	}
	return m.releaseRoot(lifecycleOperationHolderID(operationID))
}

func lifecycleTaskHolderID(taskID string) string {
	return "task:" + strings.TrimSpace(taskID)
}

func (m *lifecycleManager) AcquireTask(taskID, workspaceName string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return errors.New("execution_task_id is required for lifecycle root")
	}
	return m.acquireRoot(lifecycleRoot{
		ID: lifecycleTaskHolderID(taskID), Kind: "task", WorkspaceName: strings.TrimSpace(workspaceName),
	})
}

func (m *lifecycleManager) ReleaseTask(taskID string) (remaining int, willShutdown bool) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return m.RootCount(), false
	}
	return m.releaseRoot(lifecycleTaskHolderID(taskID))
}

func (m *lifecycleManager) scheduleIdleLocked(delay time.Duration) {
	m.generation++
	generation := m.generation
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		if m.closing || len(m.roots) != 0 || generation != m.generation {
			m.mu.Unlock()
			return
		}
		m.closing = true
		m.mu.Unlock()
		go func() { _ = m.runtime.Close() }()
	})
}

func (m *lifecycleManager) cancelIdleLocked() {
	m.generation++
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
}

func (m *lifecycleManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closing = true
	m.cancelIdleLocked()
	listener := m.listener
	path := m.socketPath
	connections := make([]net.Conn, 0, len(m.connections))
	for conn := range m.connections {
		connections = append(connections, conn)
	}
	m.listener = nil
	m.socketPath = ""
	m.mu.Unlock()
	var err error
	if listener != nil {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = closeErr
		}
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	if path != "" {
		if cleanupErr := cleanupLifecycleControlSocket(path); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}
	return err
}

func (m *lifecycleManager) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			m.mu.Lock()
			closing := m.closing || m.listener != listener
			m.mu.Unlock()
			if closing {
				return
			}
			continue
		}
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			_ = conn.Close()
			return
		}
		m.connections[conn] = true
		m.mu.Unlock()
		go m.handleConnection(conn)
	}
}

func (m *lifecycleManager) handleConnection(conn net.Conn) {
	defer conn.Close()
	defer func() {
		m.mu.Lock()
		delete(m.connections, conn)
		m.mu.Unlock()
	}()
	encoder := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 16<<10), 64<<10)
	rootID := ""
	workbenchWorkspace := workspaceView{}
	acquiredPlugins := map[string]string{}
	defer func() {
		m.releaseWorkbenchPlugins(acquiredPlugins)
		if rootID != "" {
			m.releaseRoot(rootID)
		}
	}()
	for scanner.Scan() {
		var request lifecycleControlRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "invalid lifecycle request"})
			continue
		}
		switch strings.TrimSpace(request.Type) {
		case "plugin.hello":
			if rootID != "" {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "connection already owns a Workbench"})
				continue
			}
			peer, err := m.registerPluginPeer(request.Token, request.LeaseKey, request.Plugin, request.Generation, conn, encoder)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": err.Error()})
				continue
			}
			if err := peer.send(map[string]any{"type": "plugin.ready", "ok": true, "plugin": peer.Plugin, "lease_key": peer.LeaseKey, "generation": peer.Generation}); err != nil {
				m.unregisterPluginPeer(peer)
				return
			}
			for scanner.Scan() {
				var child lifecycleControlRequest
				if err := json.Unmarshal(scanner.Bytes(), &child); err != nil {
					continue
				}
				if strings.TrimSpace(child.Generation) != peer.Generation {
					continue
				}
				switch strings.TrimSpace(child.Type) {
				case "plugin.reclaim":
					go m.runtime.requestPluginReclaim(peer.LeaseKey, peer.Generation)
				case "plugin.retain":
					m.runtime.cancelPluginReclaim(peer.LeaseKey, peer.Generation)
				}
			}
			m.unregisterPluginPeer(peer)
			return
		case "workbench.open":
			if rootID != "" {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "workbench already opened on this connection"})
				continue
			}
			ws, err := m.resolveWorkspace(request)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": err.Error()})
				continue
			}
			id, err := newLifecycleID("wb")
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": err.Error()})
				continue
			}
			rootID = "workbench:" + id
			if err := m.acquireRoot(lifecycleRoot{ID: rootID, Kind: "workbench", WorkspaceID: ws.ID, WorkspaceName: ws.Name}); err != nil {
				rootID = ""
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": err.Error()})
				continue
			}
			workbenchWorkspace = ws
			_ = encoder.Encode(map[string]any{
				"type": "workbench.ready", "ok": true, "workbench_id": id,
				"workspace_id": ws.ID, "workspace_name": ws.Name, "root_holders": m.RootCount(),
			})
		case "instance.status":
			rootHolders, workbenchHolders, closing := m.instanceStatus()
			_ = encoder.Encode(map[string]any{
				"type": "instance.status", "ok": true,
				"root_holders": rootHolders, "workbench_holders": workbenchHolders, "closing": closing,
			})
		case "workbench.status":
			if rootID == "" {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "workbench is not open"})
				continue
			}
			plugins := make([]string, 0, len(acquiredPlugins))
			for pluginName := range acquiredPlugins {
				plugins = append(plugins, pluginName)
			}
			sortStrings(plugins)
			_ = encoder.Encode(map[string]any{"type": "workbench.status", "ok": true, "root_holders": m.RootCount(), "plugins": plugins})
		case "plugin.acquire":
			if rootID == "" {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "workbench is not open"})
				continue
			}
			pluginName := strings.TrimSpace(request.Plugin)
			state, holder, err := m.acquireWorkbenchPlugin(rootID, pluginName, workbenchWorkspace)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": err.Error()})
				continue
			}
			acquiredPlugins[pluginName] = holder
			_ = encoder.Encode(map[string]any{"type": "plugin.acquired", "ok": true, "plugin": pluginName, "state": state})
		case "plugin.release":
			if rootID == "" {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "workbench is not open"})
				continue
			}
			pluginName := strings.TrimSpace(request.Plugin)
			holder, ok := acquiredPlugins[pluginName]
			if !ok {
				_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "Plugin is not acquired by this Workbench"})
				continue
			}
			m.releasePluginHolder(holder)
			delete(acquiredPlugins, pluginName)
			_ = encoder.Encode(map[string]any{"type": "plugin.released", "ok": true, "plugin": pluginName})
		case "workbench.release":
			if rootID == "" {
				rootHolders, workbenchHolders, _ := m.instanceStatus()
				_ = encoder.Encode(map[string]any{
					"type": "workbench.released", "ok": true,
					"root_holders": rootHolders, "workbench_holders": workbenchHolders, "will_shutdown": false,
				})
				continue
			}
			m.releaseWorkbenchPlugins(acquiredPlugins)
			remaining, willShutdown := m.releaseRoot(rootID)
			rootID = ""
			_, workbenchHolders, _ := m.instanceStatus()
			_ = encoder.Encode(map[string]any{
				"type": "workbench.released", "ok": true,
				"root_holders": remaining, "workbench_holders": workbenchHolders, "will_shutdown": willShutdown,
			})
		default:
			_ = encoder.Encode(map[string]any{"type": "error", "ok": false, "error": "unsupported lifecycle request"})
		}
	}
}

func (m *lifecycleManager) resolveWorkspace(request lifecycleControlRequest) (workspaceView, error) {
	name := strings.TrimSpace(request.WorkspaceName)
	if name == "" {
		return workspaceView{}, errors.New("workspace_name is required")
	}
	ws, ok := m.runtime.reg.Get(name)
	if !ok {
		return workspaceView{}, fmt.Errorf("Workspace %q is not registered", name)
	}
	if id := strings.TrimSpace(request.WorkspaceID); id != "" && id != ws.ID {
		return workspaceView{}, errors.New("workspace_id does not match registered Workspace")
	}
	if path := strings.TrimSpace(request.WorkspacePath); path != "" {
		requested, err := canonicalLifecyclePath(path)
		if err != nil {
			return workspaceView{}, errors.New("workspace_path does not match registered Workspace")
		}
		registered, err := canonicalLifecyclePath(ws.Path)
		if err != nil || requested != registered {
			return workspaceView{}, errors.New("workspace_path does not match registered Workspace")
		}
	}
	return workspaceView{ID: ws.ID, Name: ws.Name, Path: ws.Path}, nil
}

type workspaceView struct {
	ID   string
	Name string
	Path string
}

func workbenchPluginHolderID(rootID, pluginName string) string {
	return strings.TrimSpace(rootID) + ":plugin:" + strings.TrimSpace(pluginName)
}

func (m *lifecycleManager) acquireWorkbenchPlugin(rootID, pluginName string, ws workspaceView) (map[string]any, string, error) {
	if m == nil || m.runtime == nil {
		return nil, "", errors.New("lifecycle manager is unavailable")
	}
	pluginName = strings.TrimSpace(pluginName)
	if pluginName == "" {
		return nil, "", errors.New("plugin is required")
	}
	mount, ok, err := m.runtime.pluginMountForWorkspace(ws.Path, pluginName)
	if err != nil {
		return nil, "", err
	}
	if !ok || mount.Server.Plugin == nil {
		return nil, "", fmt.Errorf("Plugin %q is not registered", pluginName)
	}
	server, active, err := m.runtime.effectivePluginForWorkspace(ws.Path, pluginName)
	if err != nil {
		return nil, "", err
	}
	if !active {
		return nil, "", fmt.Errorf("Plugin %q is not enabled for Workspace %q", pluginName, ws.Name)
	}
	runtimeWorkspace := m.runtime.workspaceRuntime(ws.Name, ws.Path)
	holder := workbenchPluginHolderID(rootID, pluginName)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch mount.Server.Plugin.RuntimeType() {
	case config.PluginRuntimeNative:
		if _, err := m.runtime.controllerLeases.Acquire(ctx, holder, pluginName, server, runtimeWorkspace); err != nil {
			return nil, "", err
		}
		return m.runtime.controllerLeases.State(pluginName, runtimeWorkspace), holder, nil
	default:
		prepared, err := m.runtime.prepareMCPPluginServer(runtimeWorkspace, pluginName, server)
		if err != nil {
			return nil, "", err
		}
		if _, _, err := m.runtime.pluginLeases.Acquire(ctx, holder, pluginName, prepared, runtimeWorkspace); err != nil {
			return nil, "", err
		}
		return m.runtime.pluginLeases.State(pluginName, mount.Server.Plugin.RuntimeScope(), runtimeWorkspace), holder, nil
	}
}

func (m *lifecycleManager) releasePluginHolder(holder string) {
	if m == nil || strings.TrimSpace(holder) == "" || m.runtime == nil {
		return
	}
	if m.runtime.controllerLeases != nil {
		m.runtime.controllerLeases.ReleaseHolder(holder)
	}
	if m.runtime.pluginLeases != nil {
		m.runtime.pluginLeases.ReleaseHolder(holder)
	}
}

func (m *lifecycleManager) releaseWorkbenchPlugins(acquired map[string]string) {
	if len(acquired) == 0 {
		return
	}
	plugins := make([]string, 0, len(acquired))
	for pluginName := range acquired {
		plugins = append(plugins, pluginName)
	}
	sortStrings(plugins)
	for _, pluginName := range plugins {
		m.releasePluginHolder(acquired[pluginName])
		delete(acquired, pluginName)
	}
}

func canonicalLifecyclePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func newLifecycleID(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}
