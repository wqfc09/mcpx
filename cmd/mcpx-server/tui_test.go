package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/workspace"
)

func TestResolvedTUIPagesIncludesOnlyActivePluginPages(t *testing.T) {
	home := t.TempDir()
	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := workspace.Workspace{ID: workspace.IDForPath(workspacePath), Name: "demo", Path: workspacePath, Status: workspace.StatusOK}
	disabled, enabled := false, true
	plugin := func(scope string, tui *config.MCPPluginTUI) config.MCPServer {
		return config.MCPServer{
			Command: "node", Args: []string{"plugin.js"}, Enabled: &disabled, IsPlugin: true, Trust: true,
			Plugin: &config.MCPPlugin{Scope: scope, Tools: []string{"echo"}, Inbox: "inbox", TUI: tui},
		}
	}
	global := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"JEA":      plugin(config.PluginScopeWorkspace, &config.MCPPluginTUI{Title: "Agents", Command: "/opt/jea", Args: []string{"monitor"}, Env: map[string]string{"PLUGIN_VALUE": "yes", "MCPX_WORKSPACE": "must-not-win"}}),
		"GlobalUI": plugin(config.PluginScopeInstance, &config.MCPPluginTUI{Command: "/opt/global-ui"}),
		"Off":      plugin(config.PluginScopeWorkspace, &config.MCPPluginTUI{Command: "/opt/off"}),
		"NoTUI":    plugin(config.PluginScopeWorkspace, nil),
	}}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), global); err != nil {
		t.Fatal(err)
	}
	project := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"JEA":      {Enabled: &enabled},
		"GlobalUI": {Enabled: &enabled},
		"Off":      {Enabled: &disabled},
		"NoTUI":    {Enabled: &enabled},
		"RepoUI": {
			Command: "/opt/repo-plugin", Enabled: &enabled, IsPlugin: true, Trust: true,
			Plugin: &config.MCPPlugin{Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox", TUI: &config.MCPPluginTUI{Title: "Repo UI", Command: "/opt/repo-ui", Args: []string{"dashboard"}}},
		},
	}}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspacePath), project); err != nil {
		t.Fatal(err)
	}
	state := runtimeinstance.State{
		Version: runtimeinstance.StateVersion, InstanceID: "mcpx_demo", PID: 123,
		Executable: "/opt/mcpx", Home: home, Addr: "127.0.0.1:9090", Endpoint: "http://127.0.0.1:9090/mcp", StartedAt: runtimeinstance.StartedAtNow(),
	}
	pages, err := resolvedTUIPages(state, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || pages[0].ID != "plugin:GlobalUI" || pages[1].ID != "plugin:JEA" || pages[2].ID != "plugin:RepoUI" {
		t.Fatalf("resolved pages=%+v", pages)
	}
	globalPage := pages[0]
	if globalPage.Title != "GlobalUI" || globalPage.Env["MCPX_PLUGIN_RUNTIME_DIR"] != filepath.Join(home, "runtime", "plugins", "GlobalUI", "instance") {
		t.Fatalf("instance Plugin TUI=%+v", globalPage)
	}
	jea := pages[1]
	if jea.Title != "Agents" || jea.Command != "/opt/jea" || jea.Args[0] != "monitor" || jea.CWD != workspacePath {
		t.Fatalf("JEA TUI page=%+v", jea)
	}
	if jea.Env["PLUGIN_VALUE"] != "yes" || jea.Env["MCPX_WORKSPACE"] != workspacePath || jea.Env["MCPX_WORKSPACE_ID"] != ws.ID || jea.Env["MCPX_PLUGIN_RUNTIME_DIR"] != filepath.Join(home, "runtime", "plugins", "JEA", ws.ID) || jea.Env["MCPX_PLUGIN_TUI"] != "1" {
		t.Fatalf("JEA resolved TUI env=%+v", jea.Env)
	}
	repo := pages[2]
	if repo.Title != "Repo UI" || repo.Command != "/opt/repo-ui" || strings.Join(repo.Args, " ") != "dashboard" || repo.CWD != workspacePath || repo.Env["MCPX_PLUGIN_RUNTIME_DIR"] != filepath.Join(home, "runtime", "plugins", "RepoUI", ws.ID) {
		t.Fatalf("repo-local Plugin TUI=%+v", repo)
	}
}

func TestRenderMCPXTUIShowsRuntimeWorkspaceAndPluginState(t *testing.T) {
	home := t.TempDir()
	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := workspace.Workspace{ID: workspace.IDForPath(workspacePath), Name: "demo", Path: workspacePath, Status: workspace.StatusOK}
	disabled, enabled := false, true
	global := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"JEA": {Command: "node", Enabled: &disabled, IsPlugin: true, Plugin: &config.MCPPlugin{Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox", TUI: &config.MCPPluginTUI{Command: "jea"}}},
	}}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), global); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspacePath), config.MCPFile{MCPServers: map[string]config.MCPServer{"JEA": {Enabled: &enabled}}}); err != nil {
		t.Fatal(err)
	}
	state := runtimeinstance.State{InstanceID: "mcpx_demo", PID: 42, Endpoint: "http://127.0.0.1:9090/mcp", Home: home, Build: "test"}
	var output bytes.Buffer
	if err := renderMCPXTUI(&output, state, ws, false); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "\x1b[") {
		t.Fatalf("non-interactive TUI output must not contain ANSI escapes:\n%q", text)
	}
	for _, want := range []string{"MCPX", "demo", "RUNTIME", "WORKSPACE", "PLUGINS", "mcpx_demo", "http://127.0.0.1:9090/mcp", ws.ID, "JEA", "◌ READY", "mcp / workspace", "TUI", "Ctrl+C", "Exit MCPX page"} {
		if !strings.Contains(text, want) {
			t.Fatalf("TUI output missing %q:\n%s", want, text)
		}
	}
}

func TestRenderMCPXDashboardHostedFooterAndResponsiveWidth(t *testing.T) {
	state := runtimeinstance.State{InstanceID: "mcpx_demo", PID: 42, Endpoint: "http://127.0.0.1:9090/mcp", Build: "0.9.6", Commit: "0123456789abcdef"}
	ws := workspace.Workspace{ID: "workspace_demo", Name: "demo", Path: "/workspace/demo", Status: workspace.StatusOK}
	items := []pluginStatusItem{{Name: "JEA", Runtime: "mcp", Scope: "workspace", RuntimeState: "running", RuntimePID: 88}}
	definitions := map[string]config.MCPServer{"JEA": {IsPlugin: true, Plugin: &config.MCPPlugin{TUI: &config.MCPPluginTUI{Command: "jea"}}}}

	for _, width := range []int{55, 100, 120} {
		frame := renderMCPXDashboard(state, ws, items, definitions, width, true, "Ctrl+G", false)
		for _, line := range strings.Split(frame, "\n") {
			if got := displayWidthNoANSI(line); got > width {
				t.Fatalf("width=%d rendered line width=%d: %q", width, got, line)
			}
		}
		for _, want := range []string{"MCPX", "JEA", "Ctrl+C", "Ctrl+G", "Dashboard"} {
			if !strings.Contains(frame, want) {
				t.Fatalf("width=%d missing %q:\n%s", width, want, frame)
			}
		}
	}
}

func TestMCPXFullscreenEncodingAvoidsNewlineScrolling(t *testing.T) {
	encoded := encodeMCPXFullscreenFrame(strings.Repeat("x", 10) + "\n" + strings.Repeat("y", 10))
	if strings.Contains(encoded, "\n") {
		t.Fatalf("fullscreen encoder must use cursor addressing instead of newlines: %q", encoded)
	}
	for _, want := range []string{"\x1b[?7l", "\x1b[1;1H\x1b[2K", "\x1b[2;1H\x1b[2K", "\x1b[?7h"} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("fullscreen encoder missing %q: %q", want, encoded)
		}
	}
}

func displayWidthNoANSI(value string) int {
	return len([]rune(value))
}

func TestParseTUIOptions(t *testing.T) {
	got, err := parseTUIOptions([]string{"--workspace", "demo", "--once", "--interval", "250ms"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace != "demo" || !got.Once || got.Interval.String() != "250ms" {
		t.Fatalf("TUI options=%+v", got)
	}
}
