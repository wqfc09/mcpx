package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/pluginstate"
	"mcpx/internal/workspace"
)

type pluginStartRecord struct {
	PID         int    `json:"pid"`
	CWD         string `json:"cwd"`
	Workspace   string `json:"workspace"`
	WorkspaceID string `json:"workspace_id"`
	RuntimeDir  string `json:"runtime_dir"`
	InstanceID  string `json:"instance_id"`
}

func TestPluginBindHolderRejectsDetachedOrClosingLease(t *testing.T) {
	manager := newPluginRuntimeManager(t.TempDir(), "instance-test")
	lease := &pluginRuntimeLease{Key: "workspace:w1:reuse", Holders: map[string]bool{}}
	manager.leases[lease.Key] = lease
	lease.Closing = true
	if manager.bindHolder(lease, "session:s1") {
		t.Fatal("Closing Plugin lease accepted a new holder")
	}
	lease.Closing = false
	delete(manager.leases, lease.Key)
	if manager.bindHolder(lease, "session:s1") {
		t.Fatal("detached Plugin lease accepted a new holder")
	}
	if len(lease.Holders) != 0 {
		t.Fatalf("invalid lease gained holders: %#v", lease.Holders)
	}
}

func TestPluginReleaseDuringReplacementUpdatesHolderLedgerWithoutClosingTwice(t *testing.T) {
	manager := newPluginRuntimeManager(t.TempDir(), "instance-test")
	lease := &pluginRuntimeLease{
		Key: "workspace:w1:reuse", Closing: true,
		Holders: map[string]bool{"session:s1": true},
	}
	manager.leases[lease.Key] = lease
	if closed := manager.ReleaseHolder("session:s1"); closed != 0 {
		t.Fatalf("replacement owner must retain close ownership, closed=%d", closed)
	}
	if manager.leases[lease.Key] != lease {
		t.Fatal("Closing replacement ledger was removed before replacement installed")
	}
	if len(lease.Holders) != 0 {
		t.Fatalf("release was lost during replacement: %#v", lease.Holders)
	}
}

func TestManagedPluginCommandMatchesInterpreterReexecSafely(t *testing.T) {
	script := "/private/tmp/mcpx/plugin.py"
	pythonApp := "/Applications/Xcode.app/Contents/Developer/Library/Frameworks/Python3.framework/Versions/3.9/Resources/Python.app/Contents/MacOS/Python " + script
	if !managedPluginCommandMatches(pythonApp, "/usr/bin/python3", "python3", []string{script}) {
		t.Fatal("exact Plugin script path should survive interpreter re-exec identity change")
	}
	if managedPluginCommandMatches("/usr/bin/python /private/tmp/other.py", "/usr/bin/python3", "python3", []string{script}) {
		t.Fatal("unrelated interpreter process matched stale Plugin identity")
	}
	if managedPluginCommandMatches("/usr/bin/other serve", "/usr/bin/python3", "python3", []string{"serve"}) {
		t.Fatal("generic argument must not replace executable identity")
	}
}

func TestPluginEnsureReclaimsStalePersistedManagedProcessAfterArgsChange(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	alpha, _ := rt.reg.Get("alpha")
	server, active, err := rt.effectivePluginForWorkspace(alpha.Path, "reuse")
	if err != nil || !active {
		t.Fatalf("resolve reuse Plugin: active=%v err=%v", active, err)
	}
	if len(server.Args) == 0 {
		t.Fatal("reuse Plugin fixture is missing script argv")
	}

	staleLog := filepath.Join(home, "stale-plugin-start.jsonl")
	stale := exec.Command("python3", server.Args[0])
	stale.Dir = alpha.Path
	stale.Env = append(os.Environ(), "START_LOG="+staleLog)
	stdin, err := stale.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	identityDeadline := time.Now().Add(2 * time.Second)
	for {
		matches, matchErr := managedPluginProcessMatches(stale.Process.Pid, stale.Path, stale.Args[0], server.Args)
		if matchErr == nil && matches {
			break
		}
		if time.Now().After(identityDeadline) {
			t.Fatalf("stale Plugin process did not reach expected exec identity: matches=%v err=%v", matches, matchErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	staleDone := make(chan error, 1)
	go func() { staleDone <- stale.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		if stale.Process != nil {
			_ = stale.Process.Kill()
		}
	})

	runtimeDir := rt.pluginLeases.runtimeDir("reuse", config.PluginScopeWorkspace, alpha)
	if err := pluginstate.Write(runtimeDir, pluginstate.Status{
		Plugin: "reuse", Runtime: config.PluginRuntimeMCP, Scope: config.PluginScopeWorkspace,
		WorkspaceID: alpha.ID, WorkspaceName: alpha.Name, PID: stale.Process.Pid, Executable: stale.Path, Argv0: stale.Args[0], Args: append([]string{}, server.Args...),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), RuntimeRevision: "sha256:stale", Generation: "pg_stale",
	}); err != nil {
		t.Fatal(err)
	}
	updatedServer := server
	updatedServer.Args = append(append([]string{}, server.Args...), "--new-config")
	lease, tools, err := rt.pluginLeases.Ensure(context.Background(), "reuse", updatedServer, alpha)
	if err != nil {
		t.Fatalf("Ensure with stale persisted lease: %v", err)
	}
	if lease == nil || lease.Client == nil || len(tools) == 0 {
		t.Fatalf("fresh Plugin lease was not created: lease=%+v tools=%d", lease, len(tools))
	}
	select {
	case <-staleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stale managed Plugin process was not reclaimed")
	}
	status, err := pluginstate.Read(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if status.PID == stale.Process.Pid || status.Generation == "pg_stale" {
		t.Fatalf("stale lease survived replacement: %+v", status)
	}
	if len(status.Args) != len(updatedServer.Args) || status.Args[len(status.Args)-1] != "--new-config" {
		t.Fatalf("fresh lease did not persist current launch args: %+v", status)
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("fresh managed Plugin start records=%+v", records)
	}
}

func TestWorkspaceScopedPluginRuntimeReusesPerWorkspaceAndIsolatesAcrossWorkspaces(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	alpha, _ := rt.reg.Get("alpha")
	beta, _ := rt.reg.Get("beta")

	// Plugin definitions are loaded without starting the runtime. Business
	// processes are created only when a Workspace Session actually activates it.
	if records := readPluginStarts(t, starts); len(records) != 0 {
		t.Fatalf("MCPX startup must not probe/start Plugin runtimes: %+v", records)
	}
	for i := 0; i < 2; i++ {
		opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
		if !statusOK(opened) {
			t.Fatalf("open alpha Session %d=%+v", i, opened)
		}
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("same Workspace spawned duplicate Plugin runtimes: %+v", records)
	}

	openedBeta := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "beta"})
	if !statusOK(openedBeta) {
		t.Fatalf("open beta Session=%+v", openedBeta)
	}
	records := readPluginStarts(t, starts)
	if len(records) != 2 {
		t.Fatalf("second Workspace must get one isolated Plugin runtime: %+v", records)
	}
	byWorkspace := map[string]pluginStartRecord{}
	for _, record := range records {
		byWorkspace[record.Workspace] = record
	}
	for _, ws := range []workspace.Workspace{alpha, beta} {
		record, ok := byWorkspace[ws.Path]
		if !ok {
			t.Fatalf("missing Plugin runtime for %s: %+v", ws.Path, records)
		}
		if record.CWD != ws.Path || record.WorkspaceID != ws.ID || record.InstanceID != rt.instanceID {
			t.Fatalf("workspace Plugin launch context=%+v want ws=%+v instance=%s", record, ws, rt.instanceID)
		}
		wantRuntime := filepath.Join(home, "runtime", "plugins", "reuse", ws.ID)
		if record.RuntimeDir != wantRuntime {
			t.Fatalf("workspace Plugin runtime_dir=%q want=%q", record.RuntimeDir, wantRuntime)
		}
	}
}

func TestWorkbenchPluginReleaseStopsPageRuntimeButKeepsMCPXRoot(t *testing.T) {
	rt, _, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	state, serveErr := startPreparedLifecycleRuntime(t, rt)
	alpha, _ := rt.reg.Get("alpha")
	workbench := openWorkbenchControlForPath(t, state, "alpha", alpha.Path)
	acquired := lifecycleRequest(t, workbench, map[string]any{"type": "plugin.acquire", "plugin": "reuse"})
	if acquired["ok"] != true || acquired["type"] != "plugin.acquired" {
		t.Fatalf("plugin acquire=%+v", acquired)
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("Workbench Plugin starts=%+v", records)
	}
	if pluginState := rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha); pluginState["state"] != "running" || intValue(pluginState["holder_count"]) != 1 {
		t.Fatalf("Workbench Plugin state=%+v", pluginState)
	}
	released := lifecycleRequest(t, workbench, map[string]any{"type": "plugin.release", "plugin": "reuse"})
	if released["ok"] != true || released["type"] != "plugin.released" {
		t.Fatalf("plugin release=%+v", released)
	}
	if pluginState := rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha); pluginState["state"] != "configured" {
		t.Fatalf("page release did not stop zero-holder Plugin: %+v", pluginState)
	}
	if _, err := runtimeinstance.ResolveRunning(); err != nil {
		t.Fatalf("Workbench root should keep MCPX alive after page release: %v", err)
	}
	rootRelease := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	_ = workbench.Close()
	if rootRelease["will_shutdown"] != true {
		t.Fatalf("workbench release=%+v", rootRelease)
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestWorkbenchReleaseShutsInstanceDownDespiteRemoteSessionPluginHolder(t *testing.T) {
	rt, _, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	state, serveErr := startPreparedLifecycleRuntime(t, rt)
	alpha, _ := rt.reg.Get("alpha")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("Session open=%+v", opened)
	}
	workbench := openWorkbenchControlForPath(t, state, "alpha", alpha.Path)
	acquired := lifecycleRequest(t, workbench, map[string]any{"type": "plugin.acquire", "plugin": "reuse"})
	if acquired["ok"] != true {
		t.Fatalf("Workbench acquire=%+v", acquired)
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("Session+Workbench must share one Plugin process: %+v", records)
	}
	if pluginState := rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha); pluginState["state"] != "running" || intValue(pluginState["holder_count"]) != 2 {
		t.Fatalf("combined Plugin holders=%+v", pluginState)
	}
	workbenchRelease := lifecycleRequest(t, workbench, map[string]any{"type": "workbench.release"})
	_ = workbench.Close()
	if workbenchRelease["will_shutdown"] != true || intValue(workbenchRelease["root_holders"]) != 0 {
		t.Fatalf("Workbench release must ignore durable Session as Instance root: %+v", workbenchRelease)
	}
	waitLifecycleRuntimeExit(t, serveErr)
}

func TestWorkspacePluginRuntimeStopsOnlyAfterLastSessionHolderCloses(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	first := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	second := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(first) || !statusOK(second) {
		t.Fatalf("session opens: first=%+v second=%+v", first, second)
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("shared Plugin starts=%+v", records)
	}
	alpha, _ := rt.reg.Get("alpha")
	state := rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha)
	if state["state"] != "running" || intValue(state["holder_count"]) != 2 {
		t.Fatalf("shared Plugin state=%+v", state)
	}

	firstID := first["remote_session_id"].(string)
	closedFirst := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "close", "remote_session_id": firstID})
	if !statusOK(closedFirst) {
		t.Fatalf("close first=%+v", closedFirst)
	}
	state = rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha)
	if state["state"] != "running" || intValue(state["holder_count"]) != 1 {
		t.Fatalf("Plugin stopped while second Session still holds it: %+v", state)
	}

	secondID := second["remote_session_id"].(string)
	closedSecond := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "close", "remote_session_id": secondID})
	if !statusOK(closedSecond) {
		t.Fatalf("close second=%+v", closedSecond)
	}
	state = rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha)
	if state["state"] != "configured" {
		t.Fatalf("last Session release did not stop Plugin: %+v", state)
	}
	runtimeDir := filepath.Join(home, "runtime", "plugins", "reuse", alpha.ID)
	if _, err := os.Stat(pluginstate.Path(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("last holder release must remove Plugin lease status: %v", err)
	}
}

func TestControllerDependencyAcquireFailureRollsBackDerivedPluginHolder(t *testing.T) {
	rt, _, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	alpha, _ := rt.reg.Get("alpha")
	enabled := true
	controller := config.MCPServer{
		Command: "/bin/true", Enabled: &enabled, Trust: true, IsPlugin: true,
		Plugin: &config.MCPPlugin{Runtime: config.PluginRuntimeNative, Scope: config.PluginScopeWorkspace, Depends: []string{"reuse", "missing"}},
	}
	if _, err := rt.controllerLeases.Acquire(context.Background(), "session:rollback", "Coordinator", controller, alpha); err == nil {
		t.Fatal("Controller acquire unexpectedly succeeded with missing second dependency")
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("first dependency should have been attempted once: %+v", records)
	}
	state := rt.pluginLeases.State("reuse", config.PluginScopeWorkspace, alpha)
	if state["state"] != "configured" {
		t.Fatalf("failed Controller acquire leaked derived dependency holder: %+v", state)
	}
}

func TestWorkspacePluginRuntimePersistsAndCleansLiveLeaseStatus(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha Session=%+v", opened)
	}
	records := readPluginStarts(t, starts)
	if len(records) != 1 {
		t.Fatalf("Plugin runtime starts=%+v", records)
	}
	alpha, _ := rt.reg.Get("alpha")
	runtimeDir := filepath.Join(home, "runtime", "plugins", "reuse", alpha.ID)
	status, err := pluginstate.Read(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadMCPFile(filepath.Join(home, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if status.Plugin != "reuse" || status.Runtime != config.PluginRuntimeMCP || status.Scope != config.PluginScopeWorkspace || status.WorkspaceID != alpha.ID || status.PID != records[0].PID || status.Executable == "" || status.RuntimeRevision != config.PluginRuntimeRevision(global.MCPServers["reuse"]) {
		t.Fatalf("Plugin lease status=%+v record=%+v", status, records[0])
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pluginstate.Path(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("Runtime Close must remove Plugin lease status, err=%v", err)
	}
}

func TestPluginDefinitionUpdateInvalidatesRunningLeaseWithoutMCPXRestart(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha Session=%+v", opened)
	}
	first := readPluginStarts(t, starts)
	if len(first) != 1 {
		t.Fatalf("initial Plugin runtime starts=%+v", first)
	}
	globalPath := filepath.Join(home, ".mcp.json")
	file, err := config.LoadMCPFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	server := file.MCPServers["reuse"]
	server.Env["GENERATION"] = "v2"
	file.MCPServers["reuse"] = server
	if err := config.WriteMCPFile(globalPath, file); err != nil {
		t.Fatal(err)
	}
	opened = callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha after definition update=%+v", opened)
	}
	second := readPluginStarts(t, starts)
	if len(second) != 2 || second[0].PID == second[1].PID {
		t.Fatalf("definition update must replace the running lease without restarting MCPX: %+v", second)
	}
}

func TestPluginEnsureNeverReusesLeaseAcrossRuntimeRevision(t *testing.T) {
	rt, _, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha Session=%+v", opened)
	}
	first := readPluginStarts(t, starts)
	if len(first) != 1 {
		t.Fatalf("initial Plugin runtime starts=%+v", first)
	}
	alpha, _ := rt.reg.Get("alpha")
	server, active, err := rt.effectivePluginForWorkspace(alpha.Path, "reuse")
	if err != nil || !active {
		t.Fatalf("effective Plugin active=%v err=%v", active, err)
	}
	server.Env["GENERATION"] = "direct-v2"
	if _, _, err := rt.pluginLeases.Ensure(context.Background(), "reuse", server, alpha); err != nil {
		t.Fatal(err)
	}
	second := readPluginStarts(t, starts)
	if len(second) != 2 || second[0].PID == second[1].PID {
		t.Fatalf("Ensure reused a lease across runtime revision: first=%+v second=%+v", first, second)
	}
}

func TestPluginEnsureCanceledContextReusesHealthyLease(t *testing.T) {
	rt, _, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha Session=%+v", opened)
	}
	alpha, _ := rt.reg.Get("alpha")
	server, active, err := rt.effectivePluginForWorkspace(alpha.Path, "reuse")
	if err != nil || !active {
		t.Fatalf("effective Plugin active=%v err=%v", active, err)
	}
	first, _, err := rt.pluginLeases.Ensure(context.Background(), "reuse", server, alpha)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	second, _, err := rt.pluginLeases.Ensure(canceled, "reuse", server, alpha)
	if err != nil {
		t.Fatalf("canceled request replaced healthy Plugin: %v", err)
	}
	if first != second {
		t.Fatal("canceled request must reuse the healthy Plugin lease")
	}
	if records := readPluginStarts(t, starts); len(records) != 1 {
		t.Fatalf("canceled request restarted Plugin: %+v", records)
	}
}

func TestPluginEnsureReplacesActuallyClosedLease(t *testing.T) {
	rt, _, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha Session=%+v", opened)
	}
	alpha, _ := rt.reg.Get("alpha")
	server, active, err := rt.effectivePluginForWorkspace(alpha.Path, "reuse")
	if err != nil || !active {
		t.Fatalf("effective Plugin active=%v err=%v", active, err)
	}
	first, _, err := rt.pluginLeases.Ensure(context.Background(), "reuse", server, alpha)
	if err != nil {
		t.Fatal(err)
	}
	first.Client.Close()
	deadline := time.Now().Add(time.Second)
	for !first.Client.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !first.Client.IsClosed() {
		t.Fatal("closed Plugin connection did not report closed")
	}
	second, _, err := rt.pluginLeases.Ensure(context.Background(), "reuse", server, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("actually closed Plugin lease was reused")
	}
	if records := readPluginStarts(t, starts); len(records) != 2 {
		t.Fatalf("closed Plugin was not restarted exactly once: %+v", records)
	}
}

func TestPluginTUIOnlyUpdateKeepsRunningLease(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha Session=%+v", opened)
	}
	first := readPluginStarts(t, starts)
	if len(first) != 1 {
		t.Fatalf("initial Plugin runtime starts=%+v", first)
	}
	alpha, _ := rt.reg.Get("alpha")
	runtimeDir := filepath.Join(home, "runtime", "plugins", "reuse", alpha.ID)
	before, err := pluginstate.Read(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(home, ".mcp.json")
	file, err := config.LoadMCPFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	server := file.MCPServers["reuse"]
	plugin := *server.Plugin
	plugin.TUI = &config.MCPPluginTUI{Title: "Reuse Monitor v2", Command: "echo", Args: []string{"monitor-v2"}}
	server.Plugin = &plugin
	server.Description = "human-facing-v2"
	file.MCPServers["reuse"] = server
	if err := config.WriteMCPFile(globalPath, file); err != nil {
		t.Fatal(err)
	}
	opened = callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha after TUI update=%+v", opened)
	}
	second := readPluginStarts(t, starts)
	if len(second) != 1 || second[0].PID != first[0].PID {
		t.Fatalf("TUI-only update must not restart business Plugin runtime: first=%+v second=%+v", first, second)
	}
	after, err := pluginstate.Read(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.PID != before.PID || after.RuntimeRevision != before.RuntimeRevision {
		t.Fatalf("TUI-only update changed live lease identity: before=%+v after=%+v", before, after)
	}
	mount, ok, err := rt.pluginMountForWorkspace(alpha.Path, "reuse")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || mount.Server.Plugin.TUI == nil || mount.Server.Plugin.TUI.Title != "Reuse Monitor v2" {
		t.Fatalf("Workspace effective Plugin definition did not pick up TUI update: %+v", mount)
	}
}

func TestWorkspaceLocalPluginDefinitionDoesNotLeakAcrossWorkspaces(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	globalPath := filepath.Join(home, ".mcp.json")
	global, err := config.LoadMCPFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	server := global.MCPServers["reuse"]
	enabled := true
	server.Enabled = &enabled
	plugin := *server.Plugin
	plugin.TUI = &config.MCPPluginTUI{Title: "Repo Reuse", Command: "echo", Args: []string{"monitor"}}
	server.Plugin = &plugin
	if err := config.WriteMCPFile(globalPath, config.MCPFile{}); err != nil {
		t.Fatal(err)
	}
	alpha, ok := rt.reg.Get("alpha")
	if !ok {
		t.Fatal("alpha Workspace missing")
	}
	beta, ok := rt.reg.Get("beta")
	if !ok {
		t.Fatal("beta Workspace missing")
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(alpha.Path), config.MCPFile{MCPServers: map[string]config.MCPServer{"reuse": server}}); err != nil {
		t.Fatal(err)
	}
	alphaMount, ok, err := rt.pluginMountForWorkspace(alpha.Path, "reuse")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || alphaMount.Server.Plugin == nil || alphaMount.Server.Plugin.TUI == nil || alphaMount.Server.Plugin.TUI.Title != "Repo Reuse" {
		t.Fatalf("alpha effective Plugin=%+v", alphaMount)
	}
	if _, ok, err := rt.pluginMountForWorkspace(beta.Path, "reuse"); err != nil || ok {
		t.Fatalf("Workspace-local Plugin leaked to beta: ok=%v err=%v", ok, err)
	}

	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("open alpha=%+v", opened)
	}
	first := readPluginStarts(t, starts)
	if len(first) != 1 || first[0].Workspace != alpha.Path || first[0].WorkspaceID != alpha.ID {
		t.Fatalf("repo-local alpha Plugin runtime=%+v", first)
	}
	opened = callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "beta"})
	if !statusOK(opened) {
		t.Fatalf("open beta=%+v", opened)
	}
	if second := readPluginStarts(t, starts); len(second) != 1 {
		t.Fatalf("beta unexpectedly started alpha repo Plugin: %+v", second)
	}
}

func TestControllerRuntimeGraphRevisionTracksDependencyRuntimeOnly(t *testing.T) {
	rt, _, _ := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	alpha, ok := rt.reg.Get("alpha")
	if !ok {
		t.Fatal("alpha Workspace missing")
	}
	enabled := true
	dep := config.MCPServer{Command: "dep", Args: []string{"v1"}, Enabled: &enabled, IsPlugin: true, Trust: true, Plugin: &config.MCPPlugin{Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox"}}
	controller := config.MCPServer{Command: "controller", Enabled: &enabled, IsPlugin: true, Trust: true, Plugin: &config.MCPPlugin{Runtime: config.PluginRuntimeNative, Scope: config.PluginScopeWorkspace, Tools: []string{}, Inbox: "", Depends: []string{"dep"}, Mounts: map[string]config.MCPPluginMount{"echo": {Plugin: "dep", Tool: "echo", Automatic: true}}}}
	project := config.MCPFile{MCPServers: map[string]config.MCPServer{"dep": dep, "controller": controller}}
	if err := config.WriteMCPFile(config.ProjectMCPPath(alpha.Path), project); err != nil {
		t.Fatal(err)
	}
	merged, err := config.LoadMergedMCP(alpha.Path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := rt.pluginRuntimeGraphRevision(alpha.Path, "controller", merged.MCPServers["controller"])
	if err != nil {
		t.Fatal(err)
	}
	dep.Args = []string{"v2"}
	project.MCPServers["dep"] = dep
	if err := config.WriteMCPFile(config.ProjectMCPPath(alpha.Path), project); err != nil {
		t.Fatal(err)
	}
	merged, err = config.LoadMergedMCP(alpha.Path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.pluginRuntimeGraphRevision(alpha.Path, "controller", merged.MCPServers["controller"])
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("dependency runtime change did not change Controller graph revision: %s", first)
	}
	dep.Description = "human-facing only"
	dep.Plugin.TUI = &config.MCPPluginTUI{Command: "dep", Args: []string{"tui"}}
	project.MCPServers["dep"] = dep
	if err := config.WriteMCPFile(config.ProjectMCPPath(alpha.Path), project); err != nil {
		t.Fatal(err)
	}
	merged, err = config.LoadMergedMCP(alpha.Path)
	if err != nil {
		t.Fatal(err)
	}
	third, err := rt.pluginRuntimeGraphRevision(alpha.Path, "controller", merged.MCPServers["controller"])
	if err != nil {
		t.Fatal(err)
	}
	if third != second {
		t.Fatalf("dependency TUI/description-only change altered Controller graph revision: second=%s third=%s", second, third)
	}
}

func TestInstanceScopedPluginRuntimeReusesAcrossWorkspaces(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeInstance)
	for _, name := range []string{"alpha", "beta", "alpha"} {
		opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": name})
		if !statusOK(opened) {
			t.Fatalf("open %s Session=%+v", name, opened)
		}
	}
	records := readPluginStarts(t, starts)
	if len(records) != 1 {
		t.Fatalf("instance-scoped Plugin must use one process across Workspaces: %+v", records)
	}
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].Workspace != "" || records[0].WorkspaceID != "" || records[0].CWD != physicalHome {
		t.Fatalf("instance Plugin received Workspace-local launch context: %+v", records[0])
	}
	if records[0].RuntimeDir != filepath.Join(home, "runtime", "plugins", "reuse", "instance") || records[0].InstanceID != rt.instanceID {
		t.Fatalf("instance Plugin runtime context=%+v", records[0])
	}
}

func newPluginReuseRuntime(t *testing.T, scope string) (*Runtime, string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	rawHome, err := os.MkdirTemp("/tmp", "mcpx-plugin-reuse-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rawHome) })
	home, err := filepath.EvalSymlinks(rawHome)
	if err != nil {
		t.Fatal(err)
	}
	rawRuntime, err := os.MkdirTemp("/tmp", "mcpx-plugin-reuse-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rawRuntime) })
	runtimeDir, err := filepath.EvalSymlinks(rawRuntime)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", runtimeDir)
	alpha := filepath.Join(home, "alpha")
	beta := filepath.Join(home, "beta")
	for _, path := range []string{alpha, beta} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "alpha", Path: alpha}, {Name: "beta", Path: beta}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(home, "plugin-starts.jsonl")
	script := writePluginReuseServer(t, home)
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"reuse": {
			Command: "python3", Args: []string{script}, Trust: true, IsPlugin: true,
			Env: map[string]string{
				"START_LOG":        starts,
				"MCPX_INSTANCE_ID": "spoofed-instance",
				"MCPX_WORKSPACE":   "spoofed-workspace",
			},
			Plugin: &config.MCPPlugin{Scope: scope, Tools: []string{"echo"}, Inbox: "inbox"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{InstanceID: "mcpx_reuse_test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt, home, starts
}

func writePluginReuseServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "plugin-reuse.py")
	code := `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["START_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps({
        "pid": os.getpid(),
        "cwd": os.getcwd(),
        "workspace": os.environ.get("MCPX_WORKSPACE", ""),
        "workspace_id": os.environ.get("MCPX_WORKSPACE_ID", ""),
        "runtime_dir": os.environ.get("MCPX_PLUGIN_RUNTIME_DIR", ""),
        "instance_id": os.environ.get("MCPX_INSTANCE_ID", "")
    }, separators=(",", ":")) + "\n")
    handle.flush()

def send(message):
    sys.stdout.write(json.dumps(message, separators=(",", ":")) + "\n")
    sys.stdout.flush()

tools = [
    {"name":"echo","description":"echo","inputSchema":{"type":"object","properties":{"value":{"type":"string"}},"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}},
    {"name":"inbox","description":"inbox","inputSchema":{"type":"object","properties":{"limit":{"type":"integer"},"wait_ms":{"type":"integer"},"cursor":{"type":"string"}},"required":["limit","wait_ms"],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}}
]

for line in sys.stdin:
    try:
        request = json.loads(line)
    except Exception:
        continue
    request_id = request.get("id")
    if request_id is None:
        continue
    method = request.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":request_id,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"reuse","version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":request_id,"result":{"tools":tools}})
    elif method == "tools/call":
        params = request.get("params", {})
        name = params.get("name")
        if name == "echo":
            value = params.get("arguments", {}).get("value", "")
            send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"echo:" + value}],"isError":False}})
        elif name == "inbox":
            send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"inbox"}],"structuredContent":{"next_cursor":"next"},"isError":False}})
        else:
            send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32602,"message":"unknown tool"}})
    else:
        send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32601,"message":"method not found"}})
`
	if err := os.WriteFile(path, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readPluginStarts(t *testing.T, path string) []pluginStartRecord {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	records := make([]pluginStartRecord, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record pluginStartRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}
