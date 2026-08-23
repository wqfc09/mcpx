package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/pluginstate"
)

func TestPluginAdministrationInstallActivateUpdateDeactivate(t *testing.T) {
	home := t.TempDir()
	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{}); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeinstance.Write(runtimeinstance.State{
		Version: runtimeinstance.StateVersion, InstanceID: "mcpx_plugin_admin", PID: os.Getpid(),
		Executable: executable, Home: home, Addr: "127.0.0.1:9090", Endpoint: "http://127.0.0.1:9090/mcp", StartedAt: runtimeinstance.StartedAtNow(),
	}); err != nil {
		t.Fatal(err)
	}

	manifestPath := writePluginManifestFixture(t, home, "v1")
	code, output := captureStdout(t, func() int { return runPluginCommand([]string{"install", "--json", manifestPath}) })
	if code != 0 {
		t.Fatalf("install code=%d output=%s", code, output)
	}
	var installed map[string]any
	if err := json.Unmarshal([]byte(output), &installed); err != nil || installed["restart_required"] != false || installed["active"] != false {
		t.Fatalf("install JSON=%s err=%v", output, err)
	}
	global, err := config.LoadMCPFile(filepath.Join(home, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	definition := global.MCPServers["Demo"]
	if !definition.IsPlugin || definition.IsEnabled() || definition.Command != filepath.Join(home, "bin", "demo-v1") || definition.Args[0] != filepath.Join(home, "src", "cli-v1.js") {
		t.Fatalf("installed definition=%+v", definition)
	}

	code, output = captureStdout(t, func() int {
		return runPluginCommand([]string{"activate", "--workspace", "demo", "--json", "Demo"})
	})
	if code != 0 {
		t.Fatalf("activate code=%d output=%s", code, output)
	}
	merged, err := config.LoadMergedMCPFrom(filepath.Join(home, ".mcp.json"), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.MCPServers["Demo"].IsEnabled() {
		t.Fatal("Workspace activation did not enable installed Plugin")
	}

	code, output = captureStdout(t, func() int {
		return runPluginCommand([]string{"status", "--workspace", "demo", "--json", "Demo"})
	})
	if code != 0 {
		t.Fatalf("status code=%d output=%s", code, output)
	}
	var status struct {
		Plugins []pluginStatusItem `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil || len(status.Plugins) != 1 || status.Plugins[0].Active == nil || !*status.Plugins[0].Active || status.Plugins[0].RuntimeState != "not_running" {
		t.Fatalf("status JSON=%s err=%v", output, err)
	}

	resolvedWorkspace, err := resolvePluginWorkspace("demo")
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := resolvedWorkspace.ID
	runtimeDir, ok := pluginRuntimeDirectory(home, "Demo", definition, &resolvedWorkspace)
	if !ok {
		t.Fatal("failed to resolve managed Plugin runtime dir")
	}
	managedV1, managedV1Done := startManagedPluginTestProcess(t)
	if err := pluginstate.Write(runtimeDir, pluginstate.Status{
		Plugin: "Demo", Runtime: config.PluginRuntimeMCP, Scope: config.PluginScopeWorkspace,
		WorkspaceID: workspaceID, WorkspaceName: "demo", PID: managedV1.Process.Pid, Executable: managedV1.Path, Argv0: managedV1.Args[0],
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), RuntimeRevision: config.PluginRuntimeRevision(definition),
	}); err != nil {
		t.Fatal(err)
	}
	code, output = captureStdout(t, func() int {
		return runPluginCommand([]string{"status", "--workspace", "demo", "--json", "Demo"})
	})
	if code != 0 {
		t.Fatalf("running status code=%d output=%s", code, output)
	}
	status = struct {
		Plugins []pluginStatusItem `json:"plugins"`
	}{}
	if err := json.Unmarshal([]byte(output), &status); err != nil || len(status.Plugins) != 1 || status.Plugins[0].RuntimeState != "running" || status.Plugins[0].RuntimePID != managedV1.Process.Pid {
		t.Fatalf("running status JSON=%s err=%v", output, err)
	}

	manifestPath = writePluginManifestFixture(t, home, "v2")
	code, output = captureStdout(t, func() int { return runPluginCommand([]string{"update", "--json", manifestPath}) })
	if code != 0 {
		t.Fatalf("update code=%d output=%s", code, output)
	}
	waitManagedPluginTestProcess(t, managedV1Done)
	if _, err := os.Stat(pluginstate.Path(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("runtime-affecting update must remove old lease status, err=%v", err)
	}
	global, err = config.LoadMCPFile(filepath.Join(home, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	definition = global.MCPServers["Demo"]
	if definition.IsEnabled() || definition.Command != filepath.Join(home, "bin", "demo-v2") {
		t.Fatalf("updated Global definition=%+v", definition)
	}
	merged, err = config.LoadMergedMCPFrom(filepath.Join(home, ".mcp.json"), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.MCPServers["Demo"].IsEnabled() || merged.MCPServers["Demo"].Command != filepath.Join(home, "bin", "demo-v2") {
		t.Fatalf("Workspace activation must survive Global definition update: %+v", merged.MCPServers["Demo"])
	}
	managedV2, managedV2Done := startManagedPluginTestProcess(t)
	if err := pluginstate.Write(runtimeDir, pluginstate.Status{
		Plugin: "Demo", Runtime: config.PluginRuntimeMCP, Scope: config.PluginScopeWorkspace,
		WorkspaceID: workspaceID, WorkspaceName: "demo", PID: managedV2.Process.Pid, Executable: managedV2.Path, Argv0: managedV2.Args[0],
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), RuntimeRevision: config.PluginRuntimeRevision(definition),
	}); err != nil {
		t.Fatal(err)
	}

	code, output = captureStdout(t, func() int {
		return runPluginCommand([]string{"deactivate", "--workspace", "demo", "--json", "Demo"})
	})
	if code != 0 {
		t.Fatalf("deactivate code=%d output=%s", code, output)
	}
	waitManagedPluginTestProcess(t, managedV2Done)
	if _, err := os.Stat(pluginstate.Path(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("deactivate must remove managed lease status, err=%v", err)
	}
	merged, err = config.LoadMergedMCPFrom(filepath.Join(home, ".mcp.json"), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if merged.MCPServers["Demo"].IsEnabled() {
		t.Fatal("Workspace deactivation did not disable Plugin")
	}
}

func TestPluginValidateAndInspectPackageV2(t *testing.T) {
	home := t.TempDir()
	manifestPath := writePluginManifestFixture(t, home, "inspect")

	code, output := captureStdout(t, func() int {
		return runPluginCommand([]string{"validate", "--json", manifestPath})
	})
	if code != 0 {
		t.Fatalf("validate code=%d output=%s", code, output)
	}
	var validated map[string]any
	if err := json.Unmarshal([]byte(output), &validated); err != nil || validated["valid"] != true || validated["manifest_version"] != float64(2) || validated["runtime"] != "mcp" {
		t.Fatalf("validate JSON=%s err=%v", output, err)
	}
	if _, exists := validated["server"]; exists {
		t.Fatalf("validate must not expose internal definition: %s", output)
	}

	code, output = captureStdout(t, func() int {
		return runPluginCommand([]string{"inspect", "--json", manifestPath})
	})
	if code != 0 {
		t.Fatalf("inspect code=%d output=%s", code, output)
	}
	var inspected map[string]any
	if err := json.Unmarshal([]byte(output), &inspected); err != nil {
		t.Fatal(err)
	}
	server, _ := inspected["server"].(map[string]any)
	if server["command"] != filepath.Join(home, "bin", "demo-inspect") || server["isPlugin"] != true || server["trust"] != true || server["enabled"] != false {
		t.Fatalf("inspect resolved server=%+v", server)
	}

	brokenDir := filepath.Join(home, "broken-package")
	if err := os.MkdirAll(brokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brokenPath := filepath.Join(brokenDir, "plugin.yaml")
	if err := os.WriteFile(brokenPath, []byte("manifest_version: 2\nname: Broken\nruntime: ./runtime.yaml\ntools: ./tools.yaml\nguidence: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := captureStdout(t, func() int { return runPluginCommand([]string{"validate", "--json", brokenPath}) }); code == 0 {
		t.Fatal("validate unexpectedly accepted unknown Package V2 field")
	}
}

func TestPluginInstalledItemsExposeTUIContribution(t *testing.T) {
	enabled := true
	items := pluginInstalledItems(config.MCPFile{MCPServers: map[string]config.MCPServer{
		"WithTUI": {
			IsPlugin: true, Enabled: &enabled,
			Plugin: &config.MCPPlugin{Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox", TUI: &config.MCPPluginTUI{Command: "monitor"}},
		},
		"Headless": {
			IsPlugin: true, Enabled: &enabled,
			Plugin: &config.MCPPlugin{Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox"},
		},
	}}, nil, true)
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	byName := map[string]pluginStatusItem{}
	for _, item := range items {
		byName[item.Name] = item
	}
	if !byName["WithTUI"].TUI || byName["Headless"].TUI {
		t.Fatalf("TUI flags=%+v", byName)
	}
}

func TestPluginRuntimeAffectedNamesAcrossIncludesOldAndNewDependents(t *testing.T) {
	plugin := func(runtime string, depends ...string) config.MCPServer {
		return config.MCPServer{IsPlugin: true, Plugin: &config.MCPPlugin{Runtime: runtime, Scope: config.PluginScopeWorkspace, Depends: depends}}
	}
	oldFile := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"Demo":      plugin(config.PluginRuntimeMCP),
		"OldNative": plugin(config.PluginRuntimeNative, "Demo"),
	}}
	newFile := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"Demo":      plugin(config.PluginRuntimeMCP),
		"OldNative": plugin(config.PluginRuntimeNative),
		"NewNative": plugin(config.PluginRuntimeNative, "Demo"),
	}}
	got := pluginRuntimeAffectedNamesAcross([]config.MCPFile{oldFile, newFile}, "Demo")
	want := []string{"Demo", "NewNative", "OldNative"}
	if len(got) != len(want) {
		t.Fatalf("affected=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("affected=%v want=%v", got, want)
		}
	}
}

func TestPluginUpdateReconcileFailureLeavesOldDefinition(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeinstance.Write(runtimeinstance.State{
		Version: runtimeinstance.StateVersion, InstanceID: "mcpx_plugin_update_failure", PID: os.Getpid(),
		Executable: executable, Home: home, Addr: "127.0.0.1:9090", Endpoint: "http://127.0.0.1:9090/mcp", StartedAt: runtimeinstance.StartedAtNow(),
	}); err != nil {
		t.Fatal(err)
	}
	oldManifest, err := config.LoadPluginManifest(writePluginManifestFixture(t, home, "old"))
	if err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(home, ".mcp.json")
	if err := config.WriteMCPFile(globalPath, config.MCPFile{MCPServers: map[string]config.MCPServer{"Demo": oldManifest.Server}}); err != nil {
		t.Fatal(err)
	}
	brokenRuntime := filepath.Join(home, "runtime", "plugins", "Demo", "broken")
	if err := os.MkdirAll(brokenRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginstate.Path(brokenRuntime), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	newManifestPath := writePluginManifestFixture(t, home, "new")
	code, _ := captureStdout(t, func() int { return runPluginCommand([]string{"update", "--json", newManifestPath}) })
	if code == 0 {
		t.Fatal("update unexpectedly succeeded despite unreconcilable managed lease")
	}
	global, err := config.LoadMCPFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := global.MCPServers["Demo"].Command; got != oldManifest.Server.Command {
		t.Fatalf("failed update persisted new definition: command=%q want old=%q", got, oldManifest.Server.Command)
	}
}

func TestPluginDeactivateReconcileFailureLeavesWorkspaceActive(t *testing.T) {
	home := t.TempDir()
	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeinstance.Write(runtimeinstance.State{
		Version: runtimeinstance.StateVersion, InstanceID: "mcpx_plugin_deactivate_failure", PID: os.Getpid(),
		Executable: executable, Home: home, Addr: "127.0.0.1:9090", Endpoint: "http://127.0.0.1:9090/mcp", StartedAt: runtimeinstance.StartedAtNow(),
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := config.LoadPluginManifest(writePluginManifestFixture(t, home, "active"))
	if err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(home, ".mcp.json")
	if err := config.WriteMCPFile(globalPath, config.MCPFile{MCPServers: map[string]config.MCPServer{"Demo": manifest.Server}}); err != nil {
		t.Fatal(err)
	}
	enabled := true
	projectPath := config.ProjectMCPPath(workspacePath)
	if err := config.WriteMCPFile(projectPath, config.MCPFile{MCPServers: map[string]config.MCPServer{"Demo": {Enabled: &enabled}}}); err != nil {
		t.Fatal(err)
	}
	ws, err := resolvePluginWorkspace("demo")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir, ok := pluginRuntimeDirectory(home, "Demo", manifest.Server, &ws)
	if !ok {
		t.Fatal("failed to resolve Plugin runtime dir")
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginstate.Path(runtimeDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _ := captureStdout(t, func() int {
		return runPluginCommand([]string{"deactivate", "--workspace", "demo", "--json", "Demo"})
	})
	if code == 0 {
		t.Fatal("deactivate unexpectedly succeeded despite unreconcilable managed lease")
	}
	project, err := config.LoadMCPFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if server := project.MCPServers["Demo"]; server.Enabled == nil || !*server.Enabled {
		t.Fatalf("failed deactivate persisted disabled Workspace overlay: %+v", server)
	}
}

func TestManagedPluginChildProcess(t *testing.T) {
	if os.Getenv("MCPX_PLUGIN_TEST_CHILD") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func startManagedPluginTestProcess(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedPluginChildProcess$")
	cmd.Env = append(os.Environ(), "MCPX_PLUGIN_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return cmd, done
}

func waitManagedPluginTestProcess(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("managed Plugin test process did not stop")
	}
}

func TestParseAttachArgsSupportsJSON(t *testing.T) {
	root := t.TempDir()
	got, err := parseAttachArgs([]string{"--name", "demo", "--json", root})
	if err != nil {
		t.Fatal(err)
	}
	if !got.JSON || got.Name != "demo" || got.Path != root {
		t.Fatalf("attach options=%+v", got)
	}
}

func writePluginManifestFixture(t *testing.T, root, version string) string {
	t.Helper()
	packageDir := filepath.Join(root, "plugin-"+version)
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(packageDir, "plugin.yaml")
	runtimePath := filepath.Join(packageDir, "runtime.yaml")
	toolsPath := filepath.Join(packageDir, "tools.yaml")
	plugin := "manifest_version: 2\nname: Demo\ndescription: Demo " + version + "\nruntime: ./runtime.yaml\ntools: ./tools.yaml\n"
	runtime := "type: mcp\nscope: workspace\ncommand: ../bin/demo-" + version + "\nargs: [../src/cli-" + version + ".js, plugin]\n"
	tools := "tools:\n  - echo\ninbox: inbox\n"
	for path, body := range map[string]string{pluginPath: plugin, runtimePath: runtime, toolsPath: tools} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return pluginPath
}

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	code := fn()
	_ = writer.Close()
	os.Stdout = old
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	return code, string(body)
}
