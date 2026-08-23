//go:build !windows

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/mcpresult"
	"mcpx/internal/operation"
)

func TestWorkbenchRootsKeepMCPXAliveUntilLastRelease(t *testing.T) {
	rt, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 120*time.Millisecond)
	first := openWorkbenchControl(t, state, "demo")
	second := openWorkbenchControl(t, state, "demo")

	firstRelease := lifecycleRequest(t, first, map[string]any{"type": "workbench.release"})
	if firstRelease["will_shutdown"] != false || intValue(firstRelease["root_holders"]) != 1 {
		t.Fatalf("first release=%+v", firstRelease)
	}
	_ = first.Close()
	time.Sleep(250 * time.Millisecond)
	if _, err := runtimeinstance.ResolveRunning(); err != nil {
		t.Fatalf("one remaining Workbench must keep MCPX alive: %v", err)
	}
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("root holders=%d want=1", got)
	}

	secondRelease := lifecycleRequest(t, second, map[string]any{"type": "workbench.release"})
	if secondRelease["will_shutdown"] != true || intValue(secondRelease["root_holders"]) != 0 {
		t.Fatalf("last release=%+v", secondRelease)
	}
	_ = second.Close()
	waitLifecycleRuntimeExit(t, serveErr)
	if _, err := runtimeinstance.Read(); !errors.Is(err, runtimeinstance.ErrNotFound) {
		t.Fatalf("Instance rendezvous remained after last Workbench release: %v", err)
	}
	if _, err := os.Lstat(state.ControlSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket remained after shutdown: %v", err)
	}
}

func TestWorkbenchDisconnectAutomaticallyReleasesRoot(t *testing.T) {
	_, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 80*time.Millisecond)
	conn := openWorkbenchControl(t, state, "demo")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestReleasedWorkbenchCanObserveNewRootDuringIdleGrace(t *testing.T) {
	_, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 250*time.Millisecond)
	observer := openWorkbenchControl(t, state, "demo")
	released := lifecycleRequest(t, observer, map[string]any{"type": "workbench.release"})
	if released["will_shutdown"] != true || intValue(released["root_holders"]) != 0 {
		t.Fatalf("initial release=%+v", released)
	}
	status := lifecycleRequest(t, observer, map[string]any{"type": "instance.status"})
	if intValue(status["root_holders"]) != 0 || status["closing"] != false {
		t.Fatalf("idle status=%+v", status)
	}

	newRoot := openWorkbenchControl(t, state, "demo")
	status = lifecycleRequest(t, observer, map[string]any{"type": "instance.status"})
	if intValue(status["root_holders"]) != 1 {
		t.Fatalf("new root not visible to released observer: %+v", status)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := runtimeinstance.ResolveRunning(); err != nil {
		t.Fatalf("new root must cancel idle shutdown: %v", err)
	}
	_ = observer.Close()
	last := lifecycleRequest(t, newRoot, map[string]any{"type": "workbench.release"})
	if last["will_shutdown"] != true {
		t.Fatalf("last release=%+v", last)
	}
	_ = newRoot.Close()
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestPluginShutdownWriteRespectsGraceWhenPeerDoesNotRead(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	peer := &lifecyclePluginPeer{
		LeaseKey: "workspace:w1:JEA", Plugin: "JEA", Generation: "pg-test", Token: "pt-test",
		conn: serverConn, encoder: json.NewEncoder(serverConn), done: make(chan struct{}),
	}
	manager := &lifecycleManager{pluginPeers: map[string]*lifecyclePluginPeer{peer.LeaseKey: peer}}
	start := time.Now()
	if graceful := manager.RequestPluginShutdown(peer.LeaseKey, "idle", 40*time.Millisecond); graceful {
		t.Fatal("non-reading Plugin peer unexpectedly completed graceful shutdown")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("shutdown write exceeded bounded grace: %s", elapsed)
	}
	_ = serverConn.Close()
}

func TestPluginLifecyclePeerAuthenticatesShutsDownAndReconnectsWithoutRoot(t *testing.T) {
	rt, state, _ := startLifecycleTestRuntime(t, 5*time.Second, 2*time.Second)
	workbench := openWorkbenchControl(t, state, "demo")
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("root holders before child=%d want=1", got)
	}
	generation := "pg-test"
	socketPath, token, err := rt.lifecycle.PreparePluginPeer("workspace:demo:JEA", "JEA", generation)
	if err != nil || socketPath != state.ControlSocket || token == "" {
		t.Fatalf("prepare peer socket=%q token=%q err=%v", socketPath, token, err)
	}

	bad, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	badResponse := lifecycleRequest(t, bad, map[string]any{
		"type": "plugin.hello", "plugin": "JEA", "lease_key": "workspace:demo:JEA", "generation": generation, "token": "wrong-token",
	})
	if badResponse["ok"] != false {
		_ = bad.Close()
		t.Fatalf("invalid token accepted: %+v", badResponse)
	}
	_ = bad.Close()

	stale, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	staleResponse := lifecycleRequest(t, stale, map[string]any{
		"type": "plugin.hello", "plugin": "JEA", "lease_key": "workspace:demo:JEA", "generation": "pg-stale", "token": token,
	})
	if staleResponse["ok"] != false {
		_ = stale.Close()
		t.Fatalf("stale generation accepted: %+v", staleResponse)
	}
	_ = stale.Close()

	connect := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("unix", socketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ready := lifecycleRequest(t, conn, map[string]any{
			"type": "plugin.hello", "plugin": "JEA", "lease_key": "workspace:demo:JEA", "generation": generation, "token": token,
		})
		if ready["ok"] != true || ready["type"] != "plugin.ready" || ready["generation"] != generation {
			_ = conn.Close()
			t.Fatalf("plugin hello=%+v", ready)
		}
		return conn
	}

	peer := connect()
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("Plugin child must not become MCPX root: holders=%d", got)
	}
	shutdownDone := make(chan bool, 1)
	go func() { shutdownDone <- rt.lifecycle.RequestPluginShutdown("workspace:demo:JEA", "idle", time.Second) }()
	var shutdown map[string]any
	if err := json.NewDecoder(peer).Decode(&shutdown); err != nil {
		t.Fatal(err)
	}
	if shutdown["type"] != "plugin.shutdown" || shutdown["reason"] != "idle" || shutdown["generation"] != generation {
		t.Fatalf("shutdown frame=%+v", shutdown)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case graceful := <-shutdownDone:
		if !graceful {
			t.Fatal("MCPX did not observe Plugin peer EOF within shutdown grace")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Plugin shutdown request did not settle")
	}

	// The lease token remains valid while the lease exists so a winning process
	// can reconnect after a concurrent loser disconnected first.
	reconnected := connect()
	_ = reconnected.Close()
	time.Sleep(30 * time.Millisecond)
	rt.lifecycle.CancelPluginPeer(token)
	rootRelease := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	_ = workbench.Close()
	if rootRelease["will_shutdown"] != true {
		t.Fatalf("workbench release=%+v", rootRelease)
	}
}

func TestDuplicateRootReleaseDoesNotExtendIdleGrace(t *testing.T) {
	rt, _, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 160*time.Millisecond)
	if err := rt.lifecycle.AcquireOperation("op-idempotent", "demo"); err != nil {
		t.Fatal(err)
	}
	remaining, willShutdown := rt.lifecycle.ReleaseOperation("op-idempotent")
	if remaining != 0 || !willShutdown {
		t.Fatalf("first release remaining=%d willShutdown=%v", remaining, willShutdown)
	}
	time.Sleep(100 * time.Millisecond)
	remaining, willShutdown = rt.lifecycle.ReleaseOperation("op-idempotent")
	if remaining != 0 || willShutdown {
		t.Fatalf("duplicate release remaining=%d willShutdown=%v", remaining, willShutdown)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Runtime exited with error: %v", err)
		}
	case <-time.After(120 * time.Millisecond):
		t.Fatal("duplicate release extended idle grace")
	}
}

func TestOperationRootReleasesOnlyOnTopLevelTerminalEvent(t *testing.T) {
	rt, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 100*time.Millisecond)
	workbench := openWorkbenchControl(t, state, "demo")
	if err := rt.lifecycle.AcquireOperation("op-test", "demo"); err != nil {
		t.Fatal(err)
	}
	released := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	_ = workbench.Close()
	if released["will_shutdown"] != false || intValue(released["root_holders"]) != 1 || intValue(released["workbench_holders"]) != 0 {
		t.Fatalf("Workbench release with live Operation=%+v", released)
	}
	rt.observeOperationEvent(operation.Event{OperationID: "op-test", StepID: "step-1", State: operation.StateSucceeded})
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("step terminal event released operation root: %d", got)
	}
	rt.observeOperationEvent(operation.Event{OperationID: "op-test", State: operation.StateWaitingConfirmation})
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("waiting_confirmation released operation root: %d", got)
	}
	time.Sleep(220 * time.Millisecond)
	if _, err := runtimeinstance.ResolveRunning(); err != nil {
		t.Fatalf("nonterminal Operation must keep MCPX alive: %v", err)
	}
	rt.observeOperationEvent(operation.Event{OperationID: "op-test", State: operation.StateSucceeded})
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestTaskRootKeepsMCPXAliveAfterWorkbenchRelease(t *testing.T) {
	rt, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 100*time.Millisecond)
	workbench := openWorkbenchControl(t, state, "demo")
	if err := rt.lifecycle.AcquireTask("task-1", "demo"); err != nil {
		t.Fatal(err)
	}
	if rt.lifecycle.RootCount() != 2 {
		t.Fatalf("combined Workbench+Task roots=%d want=2", rt.lifecycle.RootCount())
	}
	released := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	if released["will_shutdown"] != false || intValue(released["root_holders"]) != 1 {
		t.Fatalf("Workbench release with live Task=%+v", released)
	}
	_ = workbench.Close()
	time.Sleep(220 * time.Millisecond)
	if _, err := runtimeinstance.ResolveRunning(); err != nil {
		t.Fatalf("Task root must keep MCPX alive: %v", err)
	}
	remaining, willShutdown := rt.lifecycle.ReleaseTask("task-1")
	if remaining != 0 || !willShutdown {
		t.Fatalf("Task release remaining=%d willShutdown=%v", remaining, willShutdown)
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestInFlightSessionRequestRootSurvivesLogicalSessionClose(t *testing.T) {
	rt, _, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 100*time.Millisecond)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)
	validated := make(chan struct{})
	releaseRequest := make(chan struct{})
	wrapped := rt.instrumentTool("session_lifecycle_race", func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_, _, _, fail := rt.changeRequest(ctx, req, false)
		if fail != nil {
			close(validated)
			return fail, nil
		}
		close(validated)
		<-releaseRequest
		return mcpresult.NewText("ok"), nil
	})
	requestDone := make(chan *mcp.CallToolResult, 1)
	go func() {
		result, _ := wrapped(context.Background(), mcpresult.Request(map[string]any{"remote_session_id": remoteID}))
		requestDone <- result
	}()
	<-validated
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("in-flight request roots=%d want=1", got)
	}

	closed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "close", "remote_session_id": remoteID})
	if !statusOK(closed) {
		t.Fatalf("session close=%+v", closed)
	}
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("in-flight request root was released by Session close: roots=%d", got)
	}
	time.Sleep(220 * time.Millisecond)
	if _, err := runtimeinstance.ResolveRunning(); err != nil {
		t.Fatalf("in-flight request must keep MCPX alive after logical close: %v", err)
	}
	close(releaseRequest)
	select {
	case result := <-requestDone:
		if result == nil || result.IsError {
			t.Fatalf("validated request failed after concurrent close: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestClosedSessionCannotBeReacquiredByOlderRequestOrSessionOpen(t *testing.T) {
	rt, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 500*time.Millisecond)
	workbench := openWorkbenchControl(t, state, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)

	unlock := rt.lifecycle.lockSession(remoteID)
	entered := make(chan struct{})
	requestDone := make(chan *mcp.CallToolResult, 1)
	wrapped := rt.instrumentTool("session_reacquire_race", func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		_, _, _, fail := rt.changeRequest(ctx, req, false)
		if fail != nil {
			return fail, nil
		}
		return mcpresult.NewText("unexpected success"), nil
	})
	go func() {
		result, _ := wrapped(context.Background(), mcpresult.Request(map[string]any{"remote_session_id": remoteID}))
		requestDone <- result
	}()
	<-entered
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	if _, err := rt.remote.Close(context.Background(), principal, remoteID, "closed"); err != nil {
		unlock()
		t.Fatal(err)
	}
	rt.lifecycle.ReleaseSession(remoteID)
	unlock()

	select {
	case result := <-requestDone:
		structured, ok := result.StructuredContent.(map[string]any)
		errorData, _ := structured["error"].(map[string]any)
		if result == nil || !ok || structured["status"] != "failed" || errorData["code"] != "SESSION_INACTIVE" {
			t.Fatalf("older request reacquired closed Session or returned wrong error: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("older request remained blocked after Session close")
	}
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("closed Session changed Workbench-only root count: %d", got)
	}
	reopened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": remoteID})
	if statusOK(reopened) {
		t.Fatalf("session(open) incorrectly reopened terminal Session: %+v", reopened)
	}
	if got := rt.lifecycle.RootCount(); got != 1 {
		t.Fatalf("terminal session(open) changed Workbench-only root count: %d", got)
	}
	released := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	_ = workbench.Close()
	if released["will_shutdown"] != true || intValue(released["root_holders"]) != 0 {
		t.Fatalf("Workbench release=%+v", released)
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestDurableRemoteSessionDoesNotKeepMCPXAliveAfterWorkbenchRelease(t *testing.T) {
	rt, state, serveErr := startLifecycleTestRuntime(t, 5*time.Second, 100*time.Millisecond)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" || rt.lifecycle.RootCount() != 0 {
		t.Fatalf("durable Session must not own Instance root: remote=%q roots=%d", remoteID, rt.lifecycle.RootCount())
	}

	workbench := openWorkbenchControl(t, state, "demo")
	if rt.lifecycle.RootCount() != 1 {
		t.Fatalf("Workbench must be the only root, got=%d", rt.lifecycle.RootCount())
	}
	released := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	_ = workbench.Close()
	if released["will_shutdown"] != true || intValue(released["root_holders"]) != 0 {
		t.Fatalf("Workbench release with durable Session=%+v", released)
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func startLifecycleTestRuntime(t *testing.T, startupGrace, idleGrace time.Duration) (*Runtime, runtimeinstance.State, <-chan error) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "mcpx-lifecycle-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	runtimeDir, err := os.MkdirTemp("/tmp", "mcpx-lifecycle-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", runtimeDir)
	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		AddrOverride: "127.0.0.1:0", InstanceID: "mcpx_lifecycle_test",
		LifecycleStartupGrace: startupGrace, LifecycleIdleGrace: idleGrace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	serveErr := make(chan error, 1)
	go func() { serveErr <- rt.Start() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		state, resolveErr := runtimeinstance.ResolveRunning()
		if resolveErr == nil {
			if state.ControlSocket == "" {
				t.Fatal("published Instance is missing control_socket")
			}
			info, statErr := os.Stat(state.ControlSocket)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("control socket mode=%o want=600", info.Mode().Perm())
			}
			return rt, state, serveErr
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Runtime did not publish lifecycle control socket: %v", resolveErr)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func openWorkbenchControl(t *testing.T, state runtimeinstance.State, workspaceName string) net.Conn {
	t.Helper()
	return openWorkbenchControlForPath(t, state, workspaceName, filepath.Join(state.Home, "workspace"))
}

func openWorkbenchControlForPath(t *testing.T, state runtimeinstance.State, workspaceName, workspacePath string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", state.ControlSocket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response := lifecycleRequest(t, conn, map[string]any{
		"type": "workbench.open", "workspace_name": workspaceName, "workspace_path": workspacePath,
	})
	if response["ok"] != true || response["type"] != "workbench.ready" {
		_ = conn.Close()
		t.Fatalf("workbench open=%+v", response)
	}
	return conn
}

func lifecycleRequest(t *testing.T, conn net.Conn, request map[string]any) map[string]any {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	return response
}

func startPreparedLifecycleRuntime(t *testing.T, rt *Runtime) (runtimeinstance.State, <-chan error) {
	t.Helper()
	rt.opts.AddrOverride = "127.0.0.1:0"
	rt.lifecycle.startupGrace = 5 * time.Second
	rt.lifecycle.idleGrace = 100 * time.Millisecond
	serveErr := make(chan error, 1)
	go func() { serveErr <- rt.Start() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		state, err := runtimeinstance.ResolveRunning()
		if err == nil && state.ControlSocket != "" {
			return state, serveErr
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Runtime did not publish lifecycle state: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitLifecycleRuntimeExit(t *testing.T, serveErr <-chan error) {
	t.Helper()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Runtime exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Runtime did not stop after lifecycle became idle")
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return -1
	}
}
