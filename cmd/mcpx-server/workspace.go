package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"mcpx/internal/config"
	"mcpx/internal/observation"
	"mcpx/internal/workspace"
)

type workspaceObserverOptions struct {
	Workspace string
	History   int
	Format    string
	Detail    bool
	Diff      string
	Tool      string
	Status    string
	Operation string
	Path      string
}

type workspaceRegisterOptions struct {
	Name string
	Path string
}

type workspacePruneOptions struct {
	Apply bool
}

func parseWorkspaceObserverArgs(args []string) (workspaceObserverOptions, error) {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	history := fs.Int("history", observation.DefaultHistory, "number of recent events to replay")
	format := fs.String("format", "text", "text|json")
	detail := fs.Bool("detail", false, "show semantic purpose, operation IDs, and execution facts")
	diff := fs.String("diff", "full", "summary|preview|full")
	tool := fs.String("tool", "", "filter by tool name")
	status := fs.String("status", "", "filter by event status")
	operation := fs.String("operation", "", "filter by operation ID")
	path := fs.String("path", "", "filter by file path")
	if err := fs.Parse(args); err != nil {
		return workspaceObserverOptions{}, err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return workspaceObserverOptions{}, fmt.Errorf("workspace name is required")
	}
	if *history <= 0 {
		return workspaceObserverOptions{}, fmt.Errorf("history must be a positive integer")
	}
	if *history > observation.MaxObserverHistory {
		*history = observation.MaxObserverHistory
	}
	*format = strings.ToLower(strings.TrimSpace(*format))
	if *format != "text" && *format != "json" {
		return workspaceObserverOptions{}, fmt.Errorf("format must be text or json")
	}
	if _, err := observation.ParseDiffMode(*diff); err != nil {
		return workspaceObserverOptions{}, err
	}
	return workspaceObserverOptions{
		Workspace: strings.TrimSpace(fs.Arg(0)), History: *history, Format: *format, Detail: *detail,
		Diff: strings.ToLower(strings.TrimSpace(*diff)), Tool: strings.TrimSpace(*tool),
		Status: strings.TrimSpace(*status), Operation: strings.TrimSpace(*operation), Path: strings.TrimSpace(*path),
	}, nil
}

func parseWorkspaceRegisterArgs(args []string) (workspaceRegisterOptions, error) {
	fs := flag.NewFlagSet("workspace register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "logical Workspace name; defaults to directory basename")
	if err := fs.Parse(args); err != nil {
		return workspaceRegisterOptions{}, err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return workspaceRegisterOptions{}, fmt.Errorf("workspace path is required")
	}
	return workspaceRegisterOptions{Name: strings.TrimSpace(*name), Path: strings.TrimSpace(fs.Arg(0))}, nil
}

func parseWorkspacePruneArgs(args []string) (workspacePruneOptions, error) {
	fs := flag.NewFlagSet("workspace prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "remove stale registry entries")
	if err := fs.Parse(args); err != nil {
		return workspacePruneOptions{}, err
	}
	if fs.NArg() != 0 {
		return workspacePruneOptions{}, fmt.Errorf("workspace prune does not accept positional arguments")
	}
	return workspacePruneOptions{Apply: *apply}, nil
}

func runWorkspaceCommand(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printWorkspaceCommandUsage(os.Stderr)
		return 0
	}
	switch args[0] {
	case "list":
		return runWorkspaceList(args[1:])
	case "register":
		return runWorkspaceRegister(args[1:])
	case "rename":
		return runWorkspaceRename(args[1:])
	case "unregister":
		return runWorkspaceUnregister(args[1:])
	case "prune":
		return runWorkspacePrune(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "workspace: unknown command %q\n", args[0])
		printWorkspaceCommandUsage(os.Stderr)
		return 2
	}
}

func openWorkspaceRegistry() (*workspace.Registry, error) {
	globalPath, err := config.GlobalConfigPath()
	if err != nil {
		return nil, err
	}
	return workspace.NewRegistry(globalPath)
}

func runWorkspaceList(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "workspace list: no arguments are accepted")
		return 2
	}
	registry, err := openWorkspaceRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace list: %v\n", err)
		return 1
	}
	items, err := registry.ListChecked()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace list: %v\n", err)
		return 1
	}
	if len(items) == 0 {
		fmt.Println("未注册 Workspace。")
		return 0
	}
	printWorkspaceRows(os.Stdout, items)
	return 0
}

func runWorkspaceRegister(args []string) int {
	options, err := parseWorkspaceRegisterArgs(args)
	if err != nil {
		if err == flag.ErrHelp {
			printWorkspaceRegisterUsage(os.Stderr)
			return 0
		}
		fmt.Fprintf(os.Stderr, "workspace register: %v\n", err)
		printWorkspaceRegisterUsage(os.Stderr)
		return 2
	}
	registry, err := openWorkspaceRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace register: %v\n", err)
		return 1
	}
	registered, err := registry.Register(options.Name, options.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace register: %v\n", err)
		return 1
	}
	fmt.Printf("已注册 Workspace：%s\n路径：%s\n", registered.Name, registered.Path)
	return 0
}

func runWorkspaceRename(args []string) int {
	if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
		fmt.Fprintln(os.Stderr, "workspace rename: old and new names are required")
		return 2
	}
	registry, err := openWorkspaceRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace rename: %v\n", err)
		return 1
	}
	renamed, err := registry.Rename(args[0], args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace rename: %v\n", err)
		return 1
	}
	fmt.Printf("已重命名 Workspace：%s\n路径：%s\n", renamed.Name, renamed.Path)
	return 0
}

func runWorkspaceUnregister(args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(os.Stderr, "workspace unregister: workspace name is required")
		return 2
	}
	registry, err := openWorkspaceRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace unregister: %v\n", err)
		return 1
	}
	removed, err := registry.Unregister(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace unregister: %v\n", err)
		return 1
	}
	fmt.Printf("已取消注册 Workspace：%s\n原路径：%s\n", removed.Name, removed.Path)
	return 0
}

func runWorkspacePrune(args []string) int {
	options, err := parseWorkspacePruneArgs(args)
	if err != nil {
		if err == flag.ErrHelp {
			printWorkspacePruneUsage(os.Stderr)
			return 0
		}
		fmt.Fprintf(os.Stderr, "workspace prune: %v\n", err)
		printWorkspacePruneUsage(os.Stderr)
		return 2
	}
	registry, err := openWorkspaceRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace prune: %v\n", err)
		return 1
	}
	stale, err := registry.Prune(options.Apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace prune: %v\n", err)
		return 1
	}
	if len(stale) == 0 {
		fmt.Println("没有 stale Workspace。")
		return 0
	}
	printWorkspaceRows(os.Stdout, stale)
	if options.Apply {
		fmt.Printf("已移除 %d 个 stale Workspace registry 条目；未修改任何 Workspace 文件。\n", len(stale))
	} else {
		fmt.Println("Dry run：使用 `mcpx workspace prune --apply` 才会移除这些 registry 条目。")
	}
	return 0
}

func printWorkspaceRows(w io.Writer, items []workspace.Workspace) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tPATH")
	for _, item := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", item.Name, item.Status, item.Path)
	}
	_ = tw.Flush()
}

func runObserve(args []string) int {
	return runObserver(args, "observe")
}

func runObserver(args []string, commandName string) int {
	options, err := parseWorkspaceObserverArgs(args)
	if err != nil {
		if err == flag.ErrHelp {
			printObserveUsage(os.Stderr)
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", commandName, err)
		printObserveUsage(os.Stderr)
		return 2
	}
	home, err := config.HomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: resolve MCPX home: %v\n", commandName, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := observation.NewClient(observation.SocketPath(home))
	isTTY := stdoutIsTTY()
	colorMode := observation.ColorModeNone
	if options.Format == "text" {
		colorMode = terminalColorMode(isTTY, os.Getenv("NO_COLOR"), os.Getenv("COLORTERM"))
	}
	color := colorMode != observation.ColorModeNone
	var textRenderer *observation.TextRenderer
	if options.Format == "text" {
		textRenderer = observation.NewTextRendererWithMode(colorMode, terminalColumns())
		textRenderer.SetDetail(options.Detail)
		diffMode, _ := observation.ParseDiffMode(options.Diff)
		textRenderer.SetDiffMode(diffMode)
		textRenderer.SetFilter(observation.EventFilter{
			Tool: options.Tool, Status: options.Status, OperationID: options.Operation, Path: options.Path,
		})
	}

	request := observation.SubscribeRequest{
		Type: "subscribe", Workspace: options.Workspace, HistoryLimit: options.History, Format: options.Format,
	}
	err = client.Run(ctx, request, func(frame observation.Frame) error {
		return renderWorkspaceFrameWithRenderer(os.Stdout, frame, options.Format, color, textRenderer)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", commandName, err)
		return 1
	}
	return 0
}

func terminalColorMode(isTTY bool, noColor, colorTerm string) observation.ColorMode {
	if !isTTY || strings.TrimSpace(noColor) != "" {
		return observation.ColorModeNone
	}
	switch strings.ToLower(strings.TrimSpace(colorTerm)) {
	case "truecolor", "24bit":
		return observation.ColorModeTrueColor
	default:
		return observation.ColorModeANSI16
	}
}

func renderWorkspaceFrame(w io.Writer, frame observation.Frame, format string, color bool) error {
	var renderer *observation.TextRenderer
	if format == "text" {
		renderer = observation.NewTextRendererWithWidth(color, terminalColumns())
	}
	return renderWorkspaceFrameWithRenderer(w, frame, format, color, renderer)
}

func renderWorkspaceFrameWithRenderer(w io.Writer, frame observation.Frame, format string, color bool, renderer *observation.TextRenderer) error {
	return renderWorkspaceFrameWithRendererAtWidth(w, frame, format, color, renderer, terminalColumns())
}

func renderWorkspaceFrameWithRendererAtWidth(w io.Writer, frame observation.Frame, format string, color bool, renderer *observation.TextRenderer, terminalWidth int) error {
	if format == "text" && renderer != nil {
		// Refresh on every frame so a terminal resize cannot leave newly-rendered
		// lines wider than the current viewport.
		renderer.SetWidth(terminalWidth)
	}
	if frame.Type == "event" && frame.Event != nil {
		if format == "json" {
			return observation.RenderJSON(w, *frame.Event)
		}
		if renderer == nil {
			renderer = observation.NewTextRendererWithWidth(color, terminalColumns())
		}
		return renderer.RenderEvent(w, *frame.Event)
	}
	if format == "json" {
		if frame.Type == "gap" || frame.Type == "error" {
			return json.NewEncoder(w).Encode(frame)
		}
		return nil
	}
	switch frame.Type {
	case "hello":
		return nil
	case "gap":
		if frame.Gap == nil {
			if _, err := fmt.Fprintln(w, "• Reconnected"); err != nil {
				return err
			}
			if renderer != nil {
				renderer.ResetAfterGap()
			}
			return nil
		}
		if _, err := fmt.Fprintln(w, "• Reconnected"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  recovered events %d-%d\n", frame.Gap.FromSequence, frame.Gap.ToSequence); err != nil {
			return err
		}
		if renderer != nil {
			renderer.ResetAfterGap()
		}
		return nil
	case "heartbeat":
		return nil
	case "error":
		if _, err := fmt.Fprintf(w, "• Failed to observe %s\n", frame.Code); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "  %s\n", frame.Message)
		return err
	default:
		_, err := fmt.Fprintf(w, "• Observed %s\n", frame.Type)
		return err
	}
}

func printObserveUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mcpx observe [flags] <workspace name>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Observe persisted Workspace events and terminal output.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -history int     recent events to replay (1-100, default 100)")
	fmt.Fprintln(w, "  -format string   text or json (default text)")
	fmt.Fprintln(w, "  -detail          show semantic purpose, operation IDs, and execution facts")
	fmt.Fprintln(w, "  -diff string     summary, preview, or full (default full)")
	fmt.Fprintln(w, "  -tool string     filter by tool name")
	fmt.Fprintln(w, "  -status string   filter by event status")
	fmt.Fprintln(w, "  -operation string filter by operation ID")
	fmt.Fprintln(w, "  -path string     filter by file path")
}

func printWorkspaceCommandUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mcpx workspace list")
	fmt.Fprintln(w, "  mcpx workspace register [--name <name>] <path>")
	fmt.Fprintln(w, "  mcpx workspace rename <old-name> <new-name>")
	fmt.Fprintln(w, "  mcpx workspace unregister <name>")
	fmt.Fprintln(w, "  mcpx workspace prune [--apply]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Manage durable Workspace registrations without starting the Runtime.")
	fmt.Fprintln(w, "unregister/prune only edit the registry; they never remove Workspace files.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "For terminal observation, use:")
	fmt.Fprintln(w, "  mcpx observe [flags] <workspace name>")
}

func printWorkspaceRegisterUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mcpx workspace register [--name <name>] <path>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Register or update a Workspace in global config. The path must be an existing directory.")
}

func printWorkspacePruneUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mcpx workspace prune [--apply]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Without --apply, only list stale registry entries. --apply removes registry entries only.")
}

func stdoutIsTTY() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func terminalColumns() int {
	columns, _ := terminalSize()
	return columns
}

func terminalSize() (columns, rows int) {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil && value > 0 {
		columns = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LINES"))); err == nil && value > 0 {
		rows = value
	}
	if columns > 0 && rows > 0 {
		return columns, rows
	}
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return columns, rows
	}
	command := exec.Command("stty", "size")
	command.Stdin = os.Stdin
	output, err := command.Output()
	if err != nil {
		return columns, rows
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return columns, rows
	}
	if rows <= 0 {
		if value, err := strconv.Atoi(fields[0]); err == nil && value > 0 {
			rows = value
		}
	}
	if columns <= 0 {
		if value, err := strconv.Atoi(fields[1]); err == nil && value > 0 {
			columns = value
		}
	}
	return columns, rows
}
