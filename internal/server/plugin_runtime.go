package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/workspace"
)

func pluginDependsOn(server config.MCPServer, names map[string]bool) bool {
	if server.Plugin == nil || server.Plugin.RuntimeType() != config.PluginRuntimeNative {
		return false
	}
	for _, raw := range server.Plugin.Depends {
		if names[strings.TrimSpace(raw)] {
			return true
		}
	}
	return false
}

// refreshPluginDefinitions reloads Global Plugin definitions without rebuilding
// the MCP server catalog. Changed definitions invalidate their owned runtime
// leases; Native runtimes that depend on a changed Plugin are also restarted on
// the next ensure so their integration contract cannot outlive dependencies.
func (r *Runtime) refreshPluginDefinitions() error {
	if r == nil || strings.TrimSpace(r.homeDir) == "" {
		return nil
	}
	next, err := discoverPluginMounts(r.cfg.Discovery.MCP.Enabled, filepath.Join(r.homeDir, ".mcp.json"))
	if err != nil {
		return err
	}
	r.pluginMu.Lock()
	defer r.pluginMu.Unlock()
	old := r.plugins
	changed := map[string]bool{}
	for name, previous := range old {
		current, ok := next[name]
		if !ok || config.PluginRuntimeRevision(previous.Server) != config.PluginRuntimeRevision(current.Server) {
			changed[name] = true
		}
	}
	for name, current := range next {
		previous, ok := old[name]
		if !ok || config.PluginRuntimeRevision(previous.Server) != config.PluginRuntimeRevision(current.Server) {
			changed[name] = true
		}
	}
	if len(changed) == 0 {
		r.plugins = next
		return nil
	}

	// Keep the definition write lock until all leases that can observe the old
	// runtime contract are invalidated. Otherwise another request can see the
	// new definition in r.plugins while Ensure still reuses an old lease.
	for name := range changed {
		if r.pluginLeases != nil {
			r.pluginLeases.InvalidatePlugin(name)
		}
		if r.controllerLeases != nil {
			r.controllerLeases.InvalidatePlugin(name)
		}
	}
	natives := map[string]config.MCPServer{}
	for name, mount := range old {
		if mount.Server.Plugin != nil && mount.Server.Plugin.RuntimeType() == config.PluginRuntimeNative {
			natives[name] = mount.Server
		}
	}
	for name, mount := range next {
		if mount.Server.Plugin != nil && mount.Server.Plugin.RuntimeType() == config.PluginRuntimeNative {
			natives[name] = mount.Server
		}
	}
	for name, server := range natives {
		if pluginDependsOn(server, changed) && r.controllerLeases != nil {
			r.controllerLeases.InvalidatePlugin(name)
		}
	}
	r.plugins = next
	return nil
}

func (r *Runtime) pluginMountByName(name string) (pluginMount, bool) {
	if r == nil {
		return pluginMount{}, false
	}
	r.pluginMu.RLock()
	defer r.pluginMu.RUnlock()
	mount, ok := r.plugins[strings.TrimSpace(name)]
	return mount, ok
}

// pluginMountsForWorkspace resolves the Plugin definition graph visible to one
// Workspace. Workspace Plugin definitions are intentionally not inserted into
// r.plugins: the Runtime can serve multiple Workspaces, so a repo-local
// definition must never leak into another Workspace's Plugin catalog.
func (r *Runtime) pluginRuntimeGraphRevision(wsPath, pluginName string, server config.MCPServer) (string, error) {
	file, err := config.LoadMergedMCP(wsPath)
	if err != nil {
		return "", err
	}
	// The caller already resolved the root definition. Preserve that exact
	// contract while taking dependencies from one effective Workspace snapshot.
	file.MCPServers[strings.TrimSpace(pluginName)] = server
	revision, graphErr := config.PluginRuntimeGraphRevision(file, pluginName)
	if graphErr != nil {
		// Runtime-manager tests and internal callers can validate a candidate
		// Native runtime before it has been persisted into a complete Plugin graph.
		// Do not let revision calculation bypass ensureDependencies' ordered
		// acquire/failure/rollback semantics. Persisted Workspace graphs are
		// already validated by LoadMergedMCP and take the full graph path above.
		return config.PluginRuntimeRevision(server), nil
	}
	return revision, nil
}

func (r *Runtime) pluginMountsForWorkspace(wsPath string) (map[string]pluginMount, error) {
	if r == nil {
		return map[string]pluginMount{}, nil
	}
	if strings.TrimSpace(wsPath) == "" {
		if err := r.refreshPluginDefinitions(); err != nil {
			return nil, err
		}
		r.pluginMu.RLock()
		mounts := make(map[string]pluginMount, len(r.plugins))
		for name, mount := range r.plugins {
			mounts[name] = mount
		}
		r.pluginMu.RUnlock()
		return mounts, nil
	}
	file, err := config.LoadMergedMCP(wsPath)
	if err != nil {
		return nil, err
	}
	return pluginMountsFromFile(r.cfg.Discovery.MCP.Enabled, file)
}

func (r *Runtime) pluginMountForWorkspace(wsPath, name string) (pluginMount, bool, error) {
	mounts, err := r.pluginMountsForWorkspace(wsPath)
	if err != nil {
		return pluginMount{}, false, err
	}
	mount, ok := mounts[strings.TrimSpace(name)]
	return mount, ok, nil
}

func (r *Runtime) pluginNamesForWorkspace(wsPath string) ([]string, error) {
	mounts, err := r.pluginMountsForWorkspace(wsPath)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(mounts))
	for name := range mounts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

type pluginMount struct {
	Name   string
	Server config.MCPServer
}

// discoverPluginMounts loads Plugin definitions only. Runtime tool schemas are
// deliberately not probed or frozen at MCPX startup: plugin_tool describe/call
// resolves the current upstream tools/list from the active Plugin lease.
func discoverPluginMounts(enabled bool, globalMCPPath string) (map[string]pluginMount, error) {
	if !enabled {
		return map[string]pluginMount{}, nil
	}
	file, err := config.LoadMCPFile(globalMCPPath)
	if err != nil {
		return nil, err
	}
	return pluginMountsFromFile(enabled, file)
}

func pluginMountsFromFile(enabled bool, file config.MCPFile) (map[string]pluginMount, error) {
	mounts := map[string]pluginMount{}
	if !enabled {
		return mounts, nil
	}
	names := make([]string, 0, len(file.MCPServers))
	for name, server := range file.MCPServers {
		if server.IsPlugin {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		definition := file.MCPServers[name]
		if err := validatePluginToolNamePart(name); err != nil {
			return nil, fmt.Errorf("Plugin %q registration name: %w", name, err)
		}
		if definition.Plugin != nil && definition.Plugin.RuntimeType() == config.PluginRuntimeMCP {
			for _, raw := range definition.Plugin.Tools {
				toolName := strings.TrimSpace(raw)
				if err := validatePluginToolNamePart(toolName); err != nil {
					return nil, fmt.Errorf("Plugin %q tool %q: %w", name, toolName, err)
				}
			}
			if inboxName := strings.TrimSpace(definition.Plugin.Inbox); inboxName != "" {
				if err := validatePluginToolNamePart(inboxName); err != nil {
					return nil, fmt.Errorf("Plugin %q inbox %q: %w", name, inboxName, err)
				}
			}
		}
		mounts[name] = pluginMount{Name: name, Server: definition}
	}
	return mounts, nil
}

func validatePluginToolNamePart(value string) error {
	if value == "" {
		return fmt.Errorf("name is empty")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("name contains invalid character %q", string(r))
	}
	return nil
}

func pluginAllowsTool(plugin *config.MCPPlugin, toolName string) bool {
	if plugin == nil || plugin.RuntimeType() != config.PluginRuntimeMCP {
		return false
	}
	toolName = strings.TrimSpace(toolName)
	for _, raw := range plugin.Tools {
		if strings.TrimSpace(raw) == toolName {
			return true
		}
	}
	return false
}

func pluginAllowedToolNames(plugin *config.MCPPlugin) []string {
	if plugin == nil || plugin.RuntimeType() != config.PluginRuntimeMCP {
		return nil
	}
	names := make([]string, 0, len(plugin.Tools))
	for _, raw := range plugin.Tools {
		if name := strings.TrimSpace(raw); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (r *Runtime) pluginInventory(ws workspace.Workspace, holder string, ctx context.Context) []map[string]any {
	ensure := strings.TrimSpace(holder) != ""
	mounts, err := r.pluginMountsForWorkspace(ws.Path)
	if err != nil {
		return []map[string]any{{"state": "error", "error": err.Error()}}
	}
	names := make([]string, 0, len(mounts))
	for name := range mounts {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		mount := mounts[name]
		item := map[string]any{
			"name": name, "description": mount.Server.Description, "installed": true,
			"runtime": mount.Server.Plugin.RuntimeType(), "scope": mount.Server.Plugin.RuntimeScope(),
			"tool_count":      len(pluginAllowedToolNames(mount.Server.Plugin)),
			"inbox_available": mount.Server.Plugin.RuntimeType() == config.PluginRuntimeNative || strings.TrimSpace(mount.Server.Plugin.Inbox) != "",
		}
		if len(mount.Server.Plugin.Depends) > 0 {
			item["depends"] = append([]string(nil), mount.Server.Plugin.Depends...)
		}
		server, active, err := r.effectivePluginForWorkspace(ws.Path, name)
		if err != nil {
			item["active"], item["state"], item["error"] = false, "error", err.Error()
			items = append(items, item)
			continue
		}
		item["active"] = active
		if !active {
			item["trust"] = mount.Server.Trust
			item["state"] = "inactive"
			items = append(items, item)
			continue
		}
		item["trust"] = server.Trust
		runtimeType := mount.Server.Plugin.RuntimeType()
		switch runtimeType {
		case config.PluginRuntimeNative:
			if ensure {
				if _, ensureErr := r.controllerLeases.Acquire(ctx, holder, name, server, ws); ensureErr != nil {
					item["state"], item["error"] = "error", ensureErr.Error()
				} else {
					for key, value := range r.controllerLeases.State(name, ws) {
						item[key] = value
					}
				}
			} else {
				for key, value := range r.controllerLeases.State(name, ws) {
					item[key] = value
				}
			}
		default:
			prepared := server
			var prepareErr error
			if ensure {
				prepared, prepareErr = r.prepareMCPPluginServer(ws, name, server)
			}
			if prepareErr != nil {
				item["state"], item["error"] = "error", prepareErr.Error()
			} else if ensure {
				if _, _, ensureErr := r.pluginLeases.Acquire(ctx, holder, name, prepared, ws); ensureErr != nil {
					item["state"], item["error"] = "error", ensureErr.Error()
				} else {
					for key, value := range r.pluginLeases.State(name, mount.Server.Plugin.RuntimeScope(), ws) {
						item[key] = value
					}
				}
			} else {
				for key, value := range r.pluginLeases.State(name, mount.Server.Plugin.RuntimeScope(), ws) {
					item[key] = value
				}
			}
		}
		items = append(items, item)
	}
	return items
}

func (r *Runtime) effectivePluginForWorkspace(wsPath, name string) (config.MCPServer, bool, error) {
	if strings.TrimSpace(wsPath) == "" {
		mount, ok := r.pluginMountByName(name)
		if !ok || !mount.Server.IsEnabled() {
			return config.MCPServer{}, false, nil
		}
		return mount.Server, true, nil
	}
	file, err := config.LoadMergedMCP(wsPath)
	if err != nil {
		return config.MCPServer{}, false, err
	}
	server, ok := file.MCPServers[name]
	if !ok || !server.IsPlugin || !server.IsEnabled() {
		return config.MCPServer{}, false, nil
	}
	return server, true, nil
}

func (r *Runtime) workspaceRuntime(name, path string) workspace.Workspace {
	if registered, ok := r.reg.Get(name); ok {
		return registered
	}
	return workspace.Workspace{Name: name, Path: path, Status: workspace.StatusMissing}
}

func (r *Runtime) activePluginNames(wsPath string) ([]string, error) {
	all, err := r.pluginNamesForWorkspace(wsPath)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, name := range all {
		_, active, err := r.effectivePluginForWorkspace(wsPath, name)
		if err != nil {
			return nil, err
		}
		if active {
			names = append(names, name)
		}
	}
	return names, nil
}

func pluginToolDescriptor(pluginName string, server config.MCPServer, upstream *mcp.Tool) map[string]any {
	if upstream == nil {
		return nil
	}
	result := map[string]any{
		"plugin":       pluginName,
		"tool":         upstream.Name,
		"description":  upstream.Description,
		"input_schema": discoverySchemaMap(upstream.InputSchema),
		"revision":     mcpRevision([]*mcp.Tool{upstream}),
		"risk":         mcpExecutionRiskForServer(server, upstream).publicData(),
	}
	if upstream.OutputSchema != nil {
		result["output_schema"] = normalizeJSONValue(upstream.OutputSchema)
	}
	if upstream.Annotations != nil {
		result["annotations"] = normalizeJSONValue(upstream.Annotations)
	}
	return result
}

func cloneMCPMeta(meta mcp.Meta) mcp.Meta {
	if meta == nil {
		return nil
	}
	copy := make(mcp.Meta, len(meta))
	for key, value := range meta {
		copy[key] = value
	}
	return copy
}

func normalizeJSONValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if json.Unmarshal(encoded, &normalized) != nil {
		return value
	}
	return normalized
}
