package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/pluginstate"
	"mcpx/internal/workspace"
)

type pluginCommandOptions struct {
	JSON      bool
	Workspace string
	Argument  string
}

type pluginStatusItem struct {
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	Installed       bool     `json:"installed"`
	Active          *bool    `json:"active,omitempty"`
	Runtime         string   `json:"runtime"`
	Scope           string   `json:"scope"`
	Tools           []string `json:"tools,omitempty"`
	Inbox           string   `json:"inbox,omitempty"`
	Dependencies    []string `json:"depends,omitempty"`
	TUI             bool     `json:"tui"`
	WorkspaceID     string   `json:"workspace_id,omitempty"`
	Workspace       string   `json:"workspace,omitempty"`
	RuntimeState    string   `json:"runtime_state"`
	RuntimePID      int      `json:"runtime_pid,omitempty"`
	RuntimeDir      string   `json:"runtime_dir,omitempty"`
	RuntimeStarted  string   `json:"runtime_started_at,omitempty"`
	RuntimeRevision string   `json:"runtime_revision,omitempty"`
}

func runPluginCommand(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printPluginCommandUsage(os.Stderr)
		return 0
	}
	switch args[0] {
	case "list":
		return runPluginList(args[1:])
	case "validate", "inspect":
		return runPluginManifestCommand(args[0], args[1:])
	case "install":
		return runPluginInstallOrUpdate("install", args[1:])
	case "update":
		return runPluginInstallOrUpdate("update", args[1:])
	case "activate":
		return runPluginActivation(true, args[1:])
	case "deactivate":
		return runPluginActivation(false, args[1:])
	case "status":
		return runPluginStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "plugin: unknown command %q\n", args[0])
		printPluginCommandUsage(os.Stderr)
		return 2
	}
}

func parsePluginArgs(command string, args []string, requireArgument bool, allowWorkspace bool) (pluginCommandOptions, error) {
	fs := flag.NewFlagSet("plugin "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "write machine-readable JSON")
	workspaceName := ""
	if allowWorkspace {
		fs.StringVar(&workspaceName, "workspace", "", "registered Workspace name; defaults to the current registered Workspace")
	}
	if err := fs.Parse(args); err != nil {
		return pluginCommandOptions{}, err
	}
	if requireArgument && (fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "") {
		return pluginCommandOptions{}, fmt.Errorf("plugin %s requires one argument", command)
	}
	if !requireArgument && fs.NArg() > 1 {
		return pluginCommandOptions{}, fmt.Errorf("plugin %s accepts at most one Plugin name", command)
	}
	argument := ""
	if fs.NArg() == 1 {
		argument = strings.TrimSpace(fs.Arg(0))
	}
	return pluginCommandOptions{JSON: *jsonOutput, Workspace: strings.TrimSpace(workspaceName), Argument: argument}, nil
}

func resolveAdministrationHome() (string, bool, error) {
	state, running, err := resolveDefaultInstance()
	if err != nil {
		return "", false, err
	}
	if running {
		return state.Home, true, nil
	}
	home, err := config.HomeDir()
	return home, false, err
}

func pluginGlobalConfig() (string, config.MCPFile, bool, error) {
	home, running, err := resolveAdministrationHome()
	if err != nil {
		return "", config.MCPFile{}, false, err
	}
	path := filepath.Join(home, ".mcp.json")
	file, err := config.LoadMCPFile(path)
	return path, file, running, err
}

func runPluginList(args []string) int {
	options, err := parsePluginArgs("list", args, false, false)
	if err != nil || options.Argument != "" {
		if err == nil {
			err = fmt.Errorf("plugin list does not accept a Plugin name")
		}
		fmt.Fprintf(os.Stderr, "plugin list: %v\n", err)
		return 2
	}
	globalPath, file, running, err := pluginGlobalConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin list: %v\n", err)
		return 1
	}
	items := pluginInstalledItems(file, nil, running)
	observePluginRuntimeItems(items, file, nil, filepath.Dir(globalPath), running)
	if options.JSON {
		return writeJSON(os.Stdout, map[string]any{"plugins": items, "instance_running": running})
	}
	printPluginRows(os.Stdout, items)
	return 0
}

func runPluginManifestCommand(action string, args []string) int {
	options, err := parsePluginArgs(action, args, true, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 2
	}
	manifest, err := config.LoadPluginManifest(options.Argument)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	result := map[string]any{
		"valid":            true,
		"manifest_version": manifest.ManifestVersion,
		"name":             manifest.Name,
		"runtime":          manifest.Server.Plugin.RuntimeType(),
		"scope":            manifest.Server.Plugin.RuntimeScope(),
	}
	if action == "inspect" {
		result["server"] = manifest.Server
	}
	if options.JSON {
		return writeJSON(os.Stdout, result)
	}
	fmt.Printf("Plugin manifest valid：%s (%s/%s)\n", manifest.Name, manifest.Server.Plugin.RuntimeType(), manifest.Server.Plugin.RuntimeScope())
	if action == "inspect" {
		fmt.Printf("Resolved command：%s\n", manifest.Server.Command)
	}
	return 0
}

func runPluginInstallOrUpdate(action string, args []string) int {
	options, err := parsePluginArgs(action, args, true, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 2
	}
	manifest, err := config.LoadPluginManifest(options.Argument)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	path, file, running, err := pluginGlobalConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	existing, exists := file.MCPServers[manifest.Name]
	if action == "install" && exists {
		fmt.Fprintf(os.Stderr, "plugin install: registration %q already exists; use `mcpx plugin update`\n", manifest.Name)
		return 1
	}
	if action == "update" && (!exists || !existing.IsPlugin) {
		fmt.Fprintf(os.Stderr, "plugin update: Plugin %q is not installed\n", manifest.Name)
		return 1
	}
	candidate := config.MergeMCP(file)
	candidate.MCPServers[manifest.Name] = manifest.Server
	if err := config.ValidateMCPFile(candidate); err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	runtimeChanged := action == "update" && config.PluginRuntimeRevision(existing) != config.PluginRuntimeRevision(manifest.Server)
	if runtimeChanged && running {
		for _, affected := range pluginRuntimeAffectedNamesAcross([]config.MCPFile{file, candidate}, manifest.Name) {
			if stopErr := stopAllManagedPluginLeases(filepath.Dir(path), affected); stopErr != nil {
				fmt.Fprintf(os.Stderr, "plugin update: reconcile %s: %v\n", affected, stopErr)
				return 1
			}
		}
	}
	if err := config.WriteMCPFile(path, candidate); err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	result := map[string]any{
		"name": manifest.Name, "action": action, "installed": true, "active": false,
		"definition_revision": config.PluginDefinitionRevision(manifest.Server),
		"instance_running":    running, "restart_required": false,
	}
	if options.JSON {
		return writeJSON(os.Stdout, result)
	}
	fmt.Printf("Plugin %s：%s\n", map[string]string{"install": "已安装", "update": "已更新"}[action], manifest.Name)
	fmt.Println("Workspace activation：disabled")
	fmt.Println("MCPX restart required：no")
	return 0
}

func runPluginActivation(active bool, args []string) int {
	action := "deactivate"
	if active {
		action = "activate"
	}
	options, err := parsePluginArgs(action, args, true, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 2
	}
	globalPath, global, running, err := pluginGlobalConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	ws, err := resolvePluginWorkspace(options.Workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	merged, err := config.LoadMergedMCPFrom(globalPath, ws.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	definition, ok := merged.MCPServers[options.Argument]
	if !ok || !definition.IsPlugin {
		fmt.Fprintf(os.Stderr, "plugin %s: Plugin %q is not installed for Workspace %q\n", action, options.Argument, ws.Name)
		return 1
	}
	projectPath := config.ProjectMCPPath(ws.Path)
	project, err := config.LoadWorkspaceMCPFile(ws.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	value := active
	if local, exists := project.MCPServers[options.Argument]; exists && local.IsPlugin && local.Plugin != nil {
		local.Enabled = &value
		project.MCPServers[options.Argument] = local
	} else {
		project.MCPServers[options.Argument] = config.MCPServer{Enabled: &value}
	}
	if err := config.ValidateWorkspaceMCPFile(project, global); err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	if !active && running {
		if err := reconcilePluginDeactivation(filepath.Dir(globalPath), globalPath, merged, definition, options.Argument, ws); err != nil {
			fmt.Fprintf(os.Stderr, "plugin deactivate: runtime reconcile: %v\n", err)
			return 1
		}
	}
	if err := config.WriteMCPFile(projectPath, project); err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: %v\n", action, err)
		return 1
	}
	// Validate the complete effective view using the running Instance Home,
	// rather than the caller's potentially different MCPX_HOME.
	if _, err := config.LoadMergedMCPFrom(globalPath, ws.Path); err != nil {
		fmt.Fprintf(os.Stderr, "plugin %s: persisted activation is invalid: %v\n", action, err)
		return 1
	}
	result := map[string]any{
		"name": options.Argument, "active": active, "workspace": ws.Name, "workspace_id": ws.ID,
		"instance_running": running, "runtime_state": map[bool]string{true: "not_running", false: "inactive"}[active],
	}
	if options.JSON {
		return writeJSON(os.Stdout, result)
	}
	state := "disabled"
	if active {
		state = "enabled"
	}
	fmt.Printf("Plugin %s：%s for Workspace %s (%s)\n", options.Argument, state, ws.Name, ws.ID)
	return 0
}

func runPluginStatus(args []string) int {
	options, err := parsePluginArgs("status", args, false, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin status: %v\n", err)
		return 2
	}
	globalPath, global, running, err := pluginGlobalConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin status: %v\n", err)
		return 1
	}
	var ws *workspace.Workspace
	if options.Workspace != "" {
		resolved, resolveErr := resolvePluginWorkspace(options.Workspace)
		if resolveErr != nil {
			fmt.Fprintf(os.Stderr, "plugin status: %v\n", resolveErr)
			return 1
		}
		ws = &resolved
	} else if resolved, resolveErr := resolvePluginWorkspace(""); resolveErr == nil {
		ws = &resolved
	}
	items := pluginInstalledItems(global, ws, running)
	if ws != nil {
		merged, mergeErr := config.LoadMergedMCPFrom(globalPath, ws.Path)
		if mergeErr != nil {
			fmt.Fprintf(os.Stderr, "plugin status: %v\n", mergeErr)
			return 1
		}
		items = pluginInstalledItems(merged, ws, running)
		for index := range items {
			server := merged.MCPServers[items[index].Name]
			active := server.IsPlugin && server.IsEnabled()
			items[index].Active = &active
			items[index].Workspace = ws.Name
			items[index].WorkspaceID = ws.ID
			if active {
				items[index].RuntimeState = "not_running"
			} else {
				items[index].RuntimeState = "inactive"
			}
		}
		observePluginRuntimeItems(items, merged, ws, filepath.Dir(globalPath), running)
	}
	if options.Argument != "" {
		filtered := items[:0]
		for _, item := range items {
			if item.Name == options.Argument {
				filtered = append(filtered, item)
			}
		}
		items = filtered
		if len(items) == 0 {
			fmt.Fprintf(os.Stderr, "plugin status: Plugin %q is not installed\n", options.Argument)
			return 1
		}
	}
	if options.JSON {
		return writeJSON(os.Stdout, map[string]any{"plugins": items, "instance_running": running})
	}
	printPluginRows(os.Stdout, items)
	return 0
}

func pluginInstalledItems(file config.MCPFile, ws *workspace.Workspace, running bool) []pluginStatusItem {
	names := make([]string, 0)
	for name, server := range file.MCPServers {
		if server.IsPlugin {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	items := make([]pluginStatusItem, 0, len(names))
	for _, name := range names {
		server := file.MCPServers[name]
		item := pluginStatusItem{
			Name: name, Description: server.Description, Installed: true,
			Runtime: server.Plugin.RuntimeType(), Scope: server.Plugin.RuntimeScope(),
			Tools: append([]string(nil), server.Plugin.Tools...), Inbox: strings.TrimSpace(server.Plugin.Inbox),
			Dependencies: append([]string(nil), server.Plugin.Depends...), TUI: server.Plugin.TUI != nil, RuntimeState: "not_observed",
		}
		if !running {
			item.RuntimeState = "instance_stopped"
		}
		if ws != nil {
			item.Workspace, item.WorkspaceID = ws.Name, ws.ID
		}
		items = append(items, item)
	}
	return items
}

func pluginRuntimeDirectory(home, name string, server config.MCPServer, ws *workspace.Workspace) (string, bool) {
	if server.Plugin == nil {
		return "", false
	}
	tail := "instance"
	if server.Plugin.RuntimeScope() == config.PluginScopeWorkspace {
		if ws == nil || strings.TrimSpace(ws.ID) == "" {
			return "", false
		}
		tail = ws.ID
	}
	return filepath.Join(home, "runtime", "plugins", name, tail), true
}

func observePluginRuntimeItems(items []pluginStatusItem, global config.MCPFile, ws *workspace.Workspace, home string, instanceRunning bool) {
	for index := range items {
		if !instanceRunning {
			items[index].RuntimeState = "instance_stopped"
			continue
		}
		server, ok := global.MCPServers[items[index].Name]
		if !ok || !server.IsPlugin {
			continue
		}
		runtimeDir, observable := pluginRuntimeDirectory(home, items[index].Name, server, ws)
		if !observable {
			continue
		}
		items[index].RuntimeDir = runtimeDir
		status, err := pluginstate.Read(runtimeDir)
		if os.IsNotExist(err) {
			if items[index].Active != nil && !*items[index].Active {
				items[index].RuntimeState = "inactive"
			} else {
				items[index].RuntimeState = "not_running"
			}
			continue
		}
		if err != nil || status.Plugin != items[index].Name || status.Scope != server.Plugin.RuntimeScope() || (server.Plugin.RuntimeScope() == config.PluginScopeWorkspace && ws != nil && status.WorkspaceID != ws.ID) {
			items[index].RuntimeState = "stale"
			continue
		}
		items[index].RuntimePID = status.PID
		items[index].RuntimeStarted = status.StartedAt
		items[index].RuntimeRevision = status.RuntimeRevision
		alive, aliveErr := backgroundProcessAlive(status.PID)
		matches := false
		if aliveErr == nil && alive {
			matches, _ = managedPluginProcessMatches(status.PID, status.Executable, status.Argv0)
		}
		expectedRevision := config.PluginRuntimeRevision(server)
		if server.Plugin.RuntimeType() == config.PluginRuntimeNative {
			graphRevision, graphErr := config.PluginRuntimeGraphRevision(global, items[index].Name)
			if graphErr != nil {
				items[index].RuntimeState = "stale"
				continue
			}
			expectedRevision = graphRevision
		}
		if aliveErr != nil || !alive || !matches || status.RuntimeRevision != expectedRevision {
			items[index].RuntimeState = "stale"
			continue
		}
		items[index].RuntimeState = "running"
	}
}

func pluginRuntimeAffectedNames(global config.MCPFile, changed string) []string {
	affected := map[string]bool{strings.TrimSpace(changed): true}
	for {
		grew := false
		for name, server := range global.MCPServers {
			if affected[name] || !server.IsPlugin || server.Plugin == nil || server.Plugin.RuntimeType() != config.PluginRuntimeNative {
				continue
			}
			for _, dep := range server.Plugin.Depends {
				if affected[strings.TrimSpace(dep)] {
					affected[name] = true
					grew = true
					break
				}
			}
		}
		if !grew {
			break
		}
	}
	names := make([]string, 0, len(affected))
	for name := range affected {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func pluginRuntimeAffectedNamesAcross(files []config.MCPFile, changed string) []string {
	seen := map[string]bool{}
	for _, file := range files {
		for _, name := range pluginRuntimeAffectedNames(file, changed) {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stopManagedPluginLease(runtimeDir, pluginName string, ws *workspace.Workspace) error {
	status, err := pluginstate.Read(runtimeDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", pluginstate.Path(runtimeDir), err)
	}
	if status.Plugin != pluginName {
		return fmt.Errorf("lease identity mismatch: got Plugin %q, want %q", status.Plugin, pluginName)
	}
	if ws != nil && status.Scope == config.PluginScopeWorkspace && status.WorkspaceID != ws.ID {
		return fmt.Errorf("lease Workspace mismatch: got %q, want %q", status.WorkspaceID, ws.ID)
	}
	alive, err := backgroundProcessAlive(status.PID)
	if err != nil {
		return err
	}
	if alive {
		if _, err := terminateManagedPluginProcess(status.PID, status.Executable, status.Argv0, 2*time.Second); err != nil {
			return err
		}
	}
	return pluginstate.Remove(runtimeDir)
}

func stopAllManagedPluginLeases(home, pluginName string) error {
	root := filepath.Join(home, "runtime", "plugins", pluginName)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := stopManagedPluginLease(filepath.Join(root, entry.Name()), pluginName, nil); err != nil {
			return err
		}
	}
	return nil
}

func pluginActiveInAnyWorkspace(globalPath, pluginName, excludedWorkspaceID string) (bool, error) {
	registry, err := openWorkspaceRegistry()
	if err != nil {
		return false, err
	}
	items, err := registry.ListChecked()
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Status != workspace.StatusOK || (excludedWorkspaceID != "" && item.ID == excludedWorkspaceID) {
			continue
		}
		merged, err := config.LoadMergedMCPFrom(globalPath, item.Path)
		if err != nil {
			return false, err
		}
		server := merged.MCPServers[pluginName]
		if server.IsPlugin && server.IsEnabled() {
			return true, nil
		}
	}
	return false, nil
}

func reconcilePluginDeactivation(home, globalPath string, definitions config.MCPFile, definition config.MCPServer, pluginName string, ws workspace.Workspace) error {
	for _, name := range pluginRuntimeAffectedNames(definitions, pluginName) {
		server := definitions.MCPServers[name]
		if !server.IsPlugin || server.Plugin == nil {
			continue
		}
		if name == pluginName && server.Plugin.RuntimeScope() == config.PluginScopeInstance {
			activeElsewhere, err := pluginActiveInAnyWorkspace(globalPath, pluginName, ws.ID)
			if err != nil {
				return err
			}
			if activeElsewhere {
				continue
			}
		}
		runtimeDir, ok := pluginRuntimeDirectory(home, name, server, &ws)
		if !ok {
			continue
		}
		if err := stopManagedPluginLease(runtimeDir, name, &ws); err != nil {
			return err
		}
	}
	_ = definition
	return nil
}

func resolvePluginWorkspace(name string) (workspace.Workspace, error) {
	registry, err := openWorkspaceRegistry()
	if err != nil {
		return workspace.Workspace{}, err
	}
	if name = strings.TrimSpace(name); name != "" {
		return registry.Resolve(name)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return workspace.Workspace{}, err
	}
	cwd = canonicalCLIPath(cwd)
	items, err := registry.ListChecked()
	if err != nil {
		return workspace.Workspace{}, err
	}
	for _, item := range items {
		if canonicalCLIPath(item.Path) == cwd && item.Status == workspace.StatusOK {
			return item, nil
		}
	}
	return workspace.Workspace{}, errors.New("current directory is not a registered Workspace; pass --workspace <name>")
}

func canonicalCLIPath(value string) string {
	abs, err := filepath.Abs(value)
	if err == nil {
		value = abs
	}
	if physical, err := filepath.EvalSymlinks(value); err == nil {
		value = physical
	}
	return filepath.Clean(value)
}

func printPluginRows(w io.Writer, items []pluginStatusItem) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tACTIVE\tRUNTIME\tSCOPE\tSTATE")
	for _, item := range items {
		active := "-"
		if item.Active != nil {
			if *item.Active {
				active = "yes"
			} else {
				active = "no"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", item.Name, active, item.Runtime, item.Scope, item.RuntimeState)
	}
	_ = tw.Flush()
}

func writeJSON(w io.Writer, value any) int {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(os.Stderr, "encode JSON: %v\n", err)
		return 1
	}
	return 0
}

func printPluginCommandUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mcpx plugin list [--json]")
	fmt.Fprintln(w, "  mcpx plugin validate [--json] <plugin.yaml|package-dir>")
	fmt.Fprintln(w, "  mcpx plugin inspect [--json] <plugin.yaml|package-dir>")
	fmt.Fprintln(w, "  mcpx plugin install [--json] <plugin.yaml|package-dir>")
	fmt.Fprintln(w, "  mcpx plugin update [--json] <plugin.yaml|package-dir>")
	fmt.Fprintln(w, "  mcpx plugin activate [--workspace NAME] [--json] <name>")
	fmt.Fprintln(w, "  mcpx plugin deactivate [--workspace NAME] [--json] <name>")
	fmt.Fprintln(w, "  mcpx plugin status [--workspace NAME] [--json] [name]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Validate/inspect parse strict Package V2 without mutation; inspect also returns the resolved internal definition. Install/update writes Global Plugin definitions; Workspace configs may also own complete normalized definitions. Activate/deactivate changes only one Workspace desired state.")
	fmt.Fprintln(w, "Plugin runtime processes remain MCPX-managed and are started/reused on demand.")
}
