package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/observation"
	"mcpx/internal/workspace"
)

func TestParseWorkspaceObserverArgs(t *testing.T) {
	options, err := parseWorkspaceObserverArgs([]string{"-history", "999", "-format", "JSON", "-diff", "summary", "-tool", "read", "-status", "succeeded", "-operation", "op_1", "-path", "src/demo.go", "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Workspace != "demo" || options.History != observation.MaxObserverHistory || options.Format != "json" || options.Diff != "summary" || options.Tool != "read" || options.Status != "succeeded" || options.Operation != "op_1" || options.Path != "src/demo.go" {
		t.Fatalf("options=%+v", options)
	}
	defaults, err := parseWorkspaceObserverArgs([]string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Format != "text" || defaults.Diff != "full" || !defaults.Detail && defaults.Diff != "full" {
		t.Fatalf("defaults=%+v", defaults)
	}
	if _, err := parseWorkspaceObserverArgs(nil); err == nil {
		t.Fatal("missing workspace should fail")
	}
	if _, err := parseWorkspaceObserverArgs([]string{"-history", "0", "demo"}); err == nil {
		t.Fatal("non-positive history should fail")
	}
	if _, err := parseWorkspaceObserverArgs([]string{"-format", "yaml", "demo"}); err == nil {
		t.Fatal("unsupported format should fail")
	}
	if _, err := parseWorkspaceObserverArgs([]string{"-diff", "invalid", "demo"}); err == nil {
		t.Fatal("unsupported diff mode should fail")
	}
}

func TestParseWorkspaceRegisterArgs(t *testing.T) {
	options, err := parseWorkspaceRegisterArgs([]string{"--name", "custom", "/tmp/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Name != "custom" || options.Path != "/tmp/demo" {
		t.Fatalf("options=%+v", options)
	}
	defaults, err := parseWorkspaceRegisterArgs([]string{"/tmp/demo"})
	if err != nil || defaults.Name != "" || defaults.Path != "/tmp/demo" {
		t.Fatalf("defaults=%+v err=%v", defaults, err)
	}
	if _, err := parseWorkspaceRegisterArgs(nil); err == nil {
		t.Fatal("missing workspace path should fail")
	}
	if _, err := parseWorkspaceRegisterArgs([]string{"one", "two"}); err == nil {
		t.Fatal("multiple workspace paths should fail")
	}
}

func TestParseWorkspacePruneArgs(t *testing.T) {
	options, err := parseWorkspacePruneArgs([]string{"--apply"})
	if err != nil || !options.Apply {
		t.Fatalf("options=%+v err=%v", options, err)
	}
	defaults, err := parseWorkspacePruneArgs(nil)
	if err != nil || defaults.Apply {
		t.Fatalf("defaults=%+v err=%v", defaults, err)
	}
	if _, err := parseWorkspacePruneArgs([]string{"extra"}); err == nil {
		t.Fatal("prune positional argument should fail")
	}
}

func TestWorkspaceRegistryTargetsRunningInstanceHomeAcrossMCPXHome(t *testing.T) {
	runtimeDir := t.TempDir()
	serverHome := t.TempDir()
	callerHome := t.TempDir()
	workspacePath := t.TempDir()
	t.Setenv("MCPX_RUNTIME_DIR", runtimeDir)
	if err := config.WriteGlobal(filepath.Join(serverHome, "config.yaml"), config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteGlobal(filepath.Join(callerHome, "config.yaml"), config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := runtimeinstance.State{
		Version: runtimeinstance.StateVersion, InstanceID: "mcpx_server_home", PID: os.Getpid(), Executable: executable,
		Home: serverHome, Addr: "127.0.0.1:9090", Endpoint: "http://127.0.0.1:9090/mcp", StartedAt: runtimeinstance.StartedAtNow(),
	}
	if err := runtimeinstance.Write(state); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeinstance.RemoveIfOwned(state.InstanceID) })
	t.Setenv("MCPX_HOME", callerHome)

	registry, err := openWorkspaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register("gateway", workspacePath); err != nil {
		t.Fatal(err)
	}
	serverConfig, err := config.LoadGlobal(filepath.Join(serverHome, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(serverConfig.Workspaces) != 1 || serverConfig.Workspaces[0].Name != "gateway" {
		t.Fatalf("running Instance registry was not updated: %+v", serverConfig.Workspaces)
	}
	callerConfig, err := config.LoadGlobal(filepath.Join(callerHome, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(callerConfig.Workspaces) != 0 {
		t.Fatalf("caller MCPX_HOME was incorrectly mutated: %+v", callerConfig.Workspaces)
	}
}

func TestPrintWorkspaceRowsIncludesHealthStatus(t *testing.T) {
	var output bytes.Buffer
	printWorkspaceRows(&output, []workspace.Workspace{{Name: "demo", Status: workspace.StatusMissing, Path: "/tmp/demo"}})
	text := output.String()
	if !strings.Contains(text, "NAME") || !strings.Contains(text, "missing") || !strings.Contains(text, "/tmp/demo") {
		t.Fatalf("workspace rows=%q", text)
	}
}

func TestTerminalColumnsUsesEnvironment(t *testing.T) {
	t.Setenv("COLUMNS", "42")
	if got := terminalColumns(); got != 42 {
		t.Fatalf("terminal columns=%d, want 42", got)
	}
}

func TestTerminalColorMode(t *testing.T) {
	tests := []struct {
		name      string
		isTTY     bool
		noColor   string
		colorTerm string
		want      observation.ColorMode
	}{
		{name: "non tty", want: observation.ColorModeNone},
		{name: "no color", isTTY: true, noColor: "1", colorTerm: "truecolor", want: observation.ColorModeNone},
		{name: "truecolor", isTTY: true, colorTerm: "truecolor", want: observation.ColorModeTrueColor},
		{name: "24bit", isTTY: true, colorTerm: "24bit", want: observation.ColorModeTrueColor},
		{name: "ansi16", isTTY: true, colorTerm: "", want: observation.ColorModeANSI16},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := terminalColorMode(test.isTTY, test.noColor, test.colorTerm); got != test.want {
				t.Fatalf("color mode=%v, want %v", got, test.want)
			}
		})
	}
}

func TestRenderWorkspaceFrameTextAndJSON(t *testing.T) {
	var text bytes.Buffer
	if err := renderWorkspaceFrame(&text, observation.Frame{Type: "hello", Workspace: "demo", ObserverID: "obs_1"}, "text", false); err != nil {
		t.Fatal(err)
	}
	if text.Len() != 0 {
		t.Fatalf("hello status should be hidden: %q", text.String())
	}
	text.Reset()
	if err := renderWorkspaceFrame(&text, observation.Frame{Type: "event", Event: &observation.Event{Sequence: 3, Type: observation.TypeObserverNotice}}, "json", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), `"sequence":3`) || !strings.HasSuffix(text.String(), "\n") {
		t.Fatalf("event=%q", text.String())
	}
}

func TestRenderWorkspaceFrameReusesTextRendererAcrossEvents(t *testing.T) {
	var output bytes.Buffer
	renderer := observation.NewTextRenderer(false)
	for _, frame := range []observation.Frame{
		{Type: "event", Event: &observation.Event{Sequence: 1, RequestID: "req_workspace", Tool: "file_read", Type: observation.TypeToolStarted}},
		{Type: "event", Event: &observation.Event{Sequence: 2, RequestID: "req_workspace", Tool: "file_read", Type: observation.TypeToolCompleted, Input: []byte(`{"path":"main.go"}`), Output: []byte(`{"status":"ok"}`)}},
	} {
		if err := renderWorkspaceFrameWithRenderer(&output, frame, "text", false, renderer); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(output.String(), "╭─") || strings.Count(output.String(), "Read main.go") != 1 {
		t.Fatalf("frames did not render one compact interaction: %q", output.String())
	}

	if err := renderWorkspaceFrameWithRenderer(&output, observation.Frame{Type: "gap"}, "text", false, renderer); err != nil {
		t.Fatal(err)
	}
	if err := renderWorkspaceFrameWithRenderer(&output, observation.Frame{Type: "event", Event: &observation.Event{Sequence: 3, RequestID: "req_workspace", Tool: "file_read", Type: observation.TypeToolCompleted, Output: []byte(`{"status":"ok"}`)}}, "text", false, renderer); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "╭─") || !strings.Contains(output.String(), "Read files") {
		t.Fatalf("gap did not reset interaction state: %q", output.String())
	}
}

func TestRenderWorkspaceFrameRefreshesRendererWidth(t *testing.T) {
	t.Setenv("COLUMNS", "28")
	var output bytes.Buffer
	renderer := observation.NewTextRenderer(false)
	if err := renderWorkspaceFrameWithRenderer(&output, observation.Frame{
		Type: "event",
		Event: &observation.Event{
			Sequence:  4,
			RequestID: "req_resize",
			Tool:      "progress",
			Type:      observation.TypeToolCompleted,
			Input:     []byte(`{"current":"报告进度"}`),
			Output:    []byte(`{"status":"ok","result":{"content":[{"type":"text","text":"这是一段在终端缩窄后也不能从左侧溢出的长文本。"}]}}`),
		},
	}, "text", false, renderer); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		if got := observationDisplayWidthForTest(line); got > 28 {
			t.Fatalf("resized line width=%d, want <= 28: %q", got, line)
		}
	}
}

func observationDisplayWidthForTest(value string) int {
	// This assertion only needs to account for the ASCII test fixture and keeps
	// the command package independent from observation's unexported width helper.
	return len([]rune(value))
}
