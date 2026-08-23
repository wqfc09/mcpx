package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/workspace"
)

type tuiPageSpec struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Kind    string            `json:"kind"`
	Plugin  string            `json:"plugin,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	CWD     string            `json:"cwd"`
	Env     map[string]string `json:"env,omitempty"`
}

type tuiOptions struct {
	Workspace string
	Once      bool
	Interval  time.Duration
}

func runTUICommand(args []string) int {
	if len(args) > 0 && args[0] == "pages" {
		return runTUIPages(args[1:])
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		printTUIUsage(os.Stderr)
		return 0
	}
	options, err := parseTUIOptions(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		printTUIUsage(os.Stderr)
		return 2
	}
	state, running, err := resolveDefaultInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		return 1
	}
	if !running {
		fmt.Fprintln(os.Stderr, "tui: MCPX default Instance is not running")
		return 1
	}
	ws, err := resolvePluginWorkspace(options.Workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		return 1
	}
	if options.Once {
		if err := renderMCPXTUI(os.Stdout, state, ws, false); err != nil {
			fmt.Fprintf(os.Stderr, "tui: %v\n", err)
			return 1
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	isTTY := stdoutIsTTY()
	if isTTY {
		fmt.Fprint(os.Stdout, "\x1b[?25l")
		defer fmt.Fprint(os.Stdout, "\x1b[?25h\x1b[0m\n")
	}
	ticker := time.NewTicker(options.Interval)
	defer ticker.Stop()
	for {
		if err := renderMCPXTUI(os.Stdout, state, ws, isTTY); err != nil {
			fmt.Fprintf(os.Stderr, "tui: %v\n", err)
			return 1
		}
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			latest, current, resolveErr := resolveDefaultInstance()
			if resolveErr == nil && current {
				state = latest
			}
		}
	}
}

func parseTUIOptions(args []string) (tuiOptions, error) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workspaceName := fs.String("workspace", "", "registered Workspace name; defaults to the current registered Workspace")
	once := fs.Bool("once", false, "render one snapshot and exit")
	interval := fs.Duration("interval", time.Second, "refresh interval")
	if err := fs.Parse(args); err != nil {
		return tuiOptions{}, err
	}
	if fs.NArg() != 0 {
		return tuiOptions{}, fmt.Errorf("tui accepts no positional arguments")
	}
	if *interval < 100*time.Millisecond {
		return tuiOptions{}, fmt.Errorf("interval must be at least 100ms")
	}
	return tuiOptions{Workspace: strings.TrimSpace(*workspaceName), Once: *once, Interval: *interval}, nil
}

func runTUIPages(args []string) int {
	fs := flag.NewFlagSet("tui pages", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workspaceName := fs.String("workspace", "", "registered Workspace name; defaults to the current registered Workspace")
	jsonOutput := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "tui pages: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "tui pages: no positional arguments are accepted")
		return 2
	}
	state, running, err := resolveDefaultInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui pages: %v\n", err)
		return 1
	}
	if !running {
		fmt.Fprintln(os.Stderr, "tui pages: MCPX default Instance is not running")
		return 1
	}
	ws, err := resolvePluginWorkspace(*workspaceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui pages: %v\n", err)
		return 1
	}
	pages, err := resolvedTUIPages(state, ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui pages: %v\n", err)
		return 1
	}
	if *jsonOutput {
		return writeJSON(os.Stdout, map[string]any{"workspace": ws, "pages": pages})
	}
	for index, page := range pages {
		fmt.Printf("%d\t%s\t%s\n", index+1, page.Title, page.ID)
	}
	return 0
}

func resolvedTUIPages(state runtimeinstance.State, ws workspace.Workspace) ([]tuiPageSpec, error) {
	pages := make([]tuiPageSpec, 0)
	globalPath := filepath.Join(state.Home, ".mcp.json")
	merged, err := config.LoadMergedMCPFrom(globalPath, ws.Path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0)
	for name, definition := range merged.MCPServers {
		if definition.IsPlugin && definition.IsEnabled() && definition.Plugin != nil && definition.Plugin.TUI != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		server := merged.MCPServers[name]
		tui := server.Plugin.TUI
		title := strings.TrimSpace(tui.Title)
		if title == "" {
			title = name
		}
		scope := server.Plugin.RuntimeScope()
		runtimeTail := "instance"
		if scope == config.PluginScopeWorkspace {
			runtimeTail = ws.ID
		}
		runtimeDir := filepath.Join(state.Home, "runtime", "plugins", name, runtimeTail)
		env := make(map[string]string, len(tui.Env)+9)
		for key, value := range tui.Env {
			env[key] = value
		}
		// MCPX-owned context wins over Plugin-provided environment. TUI pages are
		// Workspace-centric even when the business runtime itself is instance-scoped.
		env["MCPX_INSTANCE_ID"] = state.InstanceID
		env["MCPX_INSTANCE_HOME"] = state.Home
		env["MCPX_PLUGIN_NAME"] = name
		env["MCPX_PLUGIN_SCOPE"] = scope
		env["MCPX_PLUGIN_RUNTIME_DIR"] = runtimeDir
		env["MCPX_PLUGIN_TUI"] = "1"
		env["MCPX_WORKSPACE"] = ws.Path
		env["MCPX_WORKSPACE_ID"] = ws.ID
		env["MCPX_WORKSPACE_NAME"] = ws.Name
		pages = append(pages, tuiPageSpec{
			ID: "plugin:" + name, Title: title, Kind: "plugin", Plugin: name,
			Command: tui.Command, Args: append([]string(nil), tui.Args...), CWD: ws.Path, Env: env,
		})
	}
	return pages, nil
}

func renderMCPXTUI(w io.Writer, state runtimeinstance.State, ws workspace.Workspace, clear bool) error {
	globalPath := filepath.Join(state.Home, ".mcp.json")
	merged, err := config.LoadMergedMCPFrom(globalPath, ws.Path)
	if err != nil {
		return err
	}
	items := pluginInstalledItems(merged, &ws, true)
	for index := range items {
		effective := merged.MCPServers[items[index].Name]
		active := effective.IsPlugin && effective.IsEnabled()
		items[index].Active = &active
		if active {
			items[index].RuntimeState = "not_running"
		} else {
			items[index].RuntimeState = "inactive"
		}
	}
	observePluginRuntimeItems(items, merged, &ws, state.Home, true)
	width := terminalColumns()
	if width <= 0 {
		width = 100
	}
	hosted := strings.TrimSpace(os.Getenv("MCPX_LAUNCHER")) == "1"
	prefix := strings.TrimSpace(os.Getenv("MCPX_LAUNCHER_PREFIX"))
	if prefix == "" {
		prefix = "Ctrl+G"
	}
	color := clear && stdoutIsTTY() && strings.TrimSpace(os.Getenv("NO_COLOR")) == ""
	frame := renderMCPXDashboard(state, ws, items, merged.MCPServers, width, hosted, prefix, color)
	if clear {
		_, err = fmt.Fprint(w, encodeMCPXFullscreenFrame(frame))
		return err
	}
	_, err = fmt.Fprint(w, frame)
	return err
}

func printTUIUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mcpx tui [--workspace NAME] [--once] [--interval 1s]")
	fmt.Fprintln(w, "  mcpx tui pages [--workspace NAME] [--json]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "`mcpx tui` is MCPX's native whole-screen runtime page.")
	fmt.Fprintln(w, "`mcpx tui pages` resolves active Plugin TUI contributions for a TUI pager/launcher; MCPX runtime status remains available through standalone `mcpx tui`.")
}
