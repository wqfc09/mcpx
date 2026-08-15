package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/mcpproxy"
	"mcpx/internal/mcpresult"
)

const pluginToolPrefix = "plugin."

type pluginMount struct {
	Name   string
	Server config.MCPServer
	Inbox  *mcp.Tool
	Tools  map[string]*mcp.Tool
}

func discoverPluginMounts(ctx context.Context, enabled bool, globalMCPPath string) (map[string]pluginMount, error) {
	mounts := map[string]pluginMount{}
	if !enabled {
		return mounts, nil
	}
	file, err := config.LoadMCPFile(globalMCPPath)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(file.MCPServers))
	for name, server := range file.MCPServers {
		if server.IsPlugin && server.IsEnabled() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		server := file.MCPServers[name]
		if err := validatePluginToolNamePart(name); err != nil {
			return nil, fmt.Errorf("Plugin %q registration name: %w", name, err)
		}
		client, err := mcpproxy.OpenClientSession(ctx, server, nil)
		if err != nil {
			return nil, fmt.Errorf("Plugin %q connect: %w", name, err)
		}
		listed, listErr := client.ListTools(ctx)
		client.Close()
		if listErr != nil {
			return nil, fmt.Errorf("Plugin %q tools/list: %w", name, listErr)
		}
		upstream := make(map[string]*mcp.Tool, len(listed))
		for _, tool := range listed {
			if tool != nil {
				upstream[tool.Name] = tool
			}
		}
		mount := pluginMount{Name: name, Server: server, Tools: map[string]*mcp.Tool{}}
		for _, toolName := range server.Plugin.Tools {
			tool := upstream[strings.TrimSpace(toolName)]
			if tool == nil {
				return nil, fmt.Errorf("Plugin %q configured tool %q was not returned by tools/list", name, toolName)
			}
			publicName := mountedPluginToolName(name, tool.Name)
			if len(publicName) > 128 {
				return nil, fmt.Errorf("Plugin %q mounted tool name %q exceeds 128 characters", name, publicName)
			}
			if err := validatePluginToolNamePart(tool.Name); err != nil {
				return nil, fmt.Errorf("Plugin %q tool %q: %w", name, tool.Name, err)
			}
			copy := *tool
			mount.Tools[tool.Name] = &copy
		}
		inboxName := strings.TrimSpace(server.Plugin.Inbox)
		if upstream[inboxName] == nil {
			return nil, fmt.Errorf("Plugin %q inbox %q was not returned by tools/list", name, inboxName)
		}
		inboxCopy := *upstream[inboxName]
		mount.Inbox = &inboxCopy
		mounts[name] = mount
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

func mountedPluginToolName(pluginName, upstreamToolName string) string {
	return pluginToolPrefix + pluginName + "." + upstreamToolName
}

func (r *Runtime) registerMountedPluginTools(s *mcp.Server) {
	pluginNames := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		pluginNames = append(pluginNames, name)
	}
	sort.Strings(pluginNames)
	for _, pluginName := range pluginNames {
		mount := r.plugins[pluginName]
		toolNames := make([]string, 0, len(mount.Tools))
		for name := range mount.Tools {
			toolNames = append(toolNames, name)
		}
		sort.Strings(toolNames)
		for _, upstreamName := range toolNames {
			upstream := mount.Tools[upstreamName]
			tool := *upstream
			tool.Name = mountedPluginToolName(pluginName, upstreamName)
			tool.InputSchema = mountedPluginInputSchema(upstream.InputSchema)
			pluginNameCopy, upstreamNameCopy := pluginName, upstreamName
			r.addMountedPluginTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return r.callMountedPluginTool(ctx, req, pluginNameCopy, upstreamNameCopy)
			})
		}
	}
}

func (r *Runtime) pluginInventory() []map[string]any {
	names := r.sortedPluginNames()
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		mount := r.plugins[name]
		items = append(items, map[string]any{
			"name": name, "description": mount.Server.Description, "trust": mount.Server.Trust,
			"mounted_tool_count": len(mount.Tools),
		})
	}
	return items
}

func (r *Runtime) addMountedPluginTool(s *mcp.Server, tool mcp.Tool, handler mcp.ToolHandler) {
	instrumented := r.instrumentTool(tool.Name, handler)
	if r.toolHandlers == nil {
		r.toolHandlers = map[string]mcp.ToolHandler{}
	}
	if r.toolMeta == nil {
		r.toolMeta = map[string]toolAnnotation{}
	}
	if r.toolIndex == nil {
		r.toolIndex = map[string]mcp.Tool{}
	}
	annotation := toolAnnotation{}
	if tool.Annotations != nil {
		annotation = toolAnnotation{
			ReadOnly: tool.Annotations.ReadOnlyHint, Destructive: boolPointerValue(tool.Annotations.DestructiveHint),
			Idempotent: tool.Annotations.IdempotentHint, OpenWorld: boolPointerValue(tool.Annotations.OpenWorldHint),
		}
	}
	r.toolHandlers[tool.Name] = instrumented
	r.toolMeta[tool.Name] = annotation
	r.toolIndexMu.Lock()
	r.toolIndex[tool.Name] = tool
	r.toolIndexMu.Unlock()
	copy := tool
	s.AddTool(&copy, instrumented)
}

func (r *Runtime) callMountedPluginTool(ctx context.Context, req *mcp.CallToolRequest, pluginName, upstreamToolName string) (*mcp.CallToolResult, error) {
	mount, ok := r.plugins[pluginName]
	if !ok || mount.Tools[upstreamToolName] == nil {
		return mcpresult.NewError(fmt.Sprintf("PLUGIN_TOOL_NOT_FOUND: mounted Plugin tool %q is unavailable", mountedPluginToolName(pluginName, upstreamToolName))), nil
	}
	publicArgs := mcpresult.Arguments(req)
	arguments, ok := publicArgs["arguments"].(map[string]any)
	if !ok && publicArgs["arguments"] != nil {
		return mcpresult.NewError("PLUGIN_ARGUMENT_INVALID: arguments must be an object"), nil
	}
	syntheticArguments := map[string]any{
		"action":            "call",
		"remote_session_id": strings.TrimSpace(stringPayload(publicArgs, "remote_session_id")),
		"purpose":           strings.TrimSpace(stringPayload(publicArgs, "purpose")),
		"server":            pluginName,
		"tool":              upstreamToolName,
		"arguments":         arguments,
	}
	if confirmed, ok := publicArgs["user_confirmed"].(bool); ok {
		syntheticArguments["user_confirmed"] = confirmed
	}
	if key := strings.TrimSpace(stringPayload(publicArgs, "idempotency_key")); key != "" {
		syntheticArguments["idempotency_key"] = key
	}
	synthetic := mcpresult.Request(syntheticArguments)
	if req != nil {
		synthetic.Session = req.Session
		synthetic.Extra = req.Extra
		if synthetic.Params != nil && req.Params != nil {
			synthetic.Params.Meta = cloneMCPMeta(req.Params.Meta)
		}
	}
	expectedRevision := mcpRevision([]*mcp.Tool{mount.Tools[upstreamToolName]})
	server := mount.Server
	return r.mcpToolCallWithServer(ctx, synthetic, mountedPluginToolName(pluginName, upstreamToolName), &server, expectedRevision)
}

func mountedPluginInputSchema(upstream any) json.RawMessage {
	upstreamSchema := discoverySchemaMap(upstream)
	if len(upstreamSchema) == 0 {
		upstreamSchema = map[string]any{"type": "object", "additionalProperties": true}
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"remote_session_id": stringSchema("跨客户端复用的 Remote Session 标识"),
			"purpose":           stringSchema("调用该 Plugin 能力的用户目标"),
			"arguments":         upstreamSchema,
			"user_confirmed":    booleanSchema("用户已确认同一 Plugin 调用"),
			"idempotency_key":   stringSchema("同一 Plugin 调用重试时复用的幂等键"),
		},
		"required":             []string{"remote_session_id", "purpose", "arguments"},
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schema)
	return json.RawMessage(raw)
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

func pluginToolDescriptor(mount pluginMount, upstreamName string) map[string]any {
	upstream := mount.Tools[upstreamName]
	if upstream == nil {
		return nil
	}
	result := map[string]any{
		"plugin":                mount.Name,
		"name":                  mountedPluginToolName(mount.Name, upstreamName),
		"upstream_tool":         upstreamName,
		"description":           upstream.Description,
		"input_schema":          normalizeJSONValue(mountedPluginInputSchema(upstream.InputSchema)),
		"upstream_input_schema": discoverySchemaMap(upstream.InputSchema),
		"revision":              mcpRevision([]*mcp.Tool{upstream}),
		"usage": map[string]any{
			"arguments": "Pass the original upstream tool arguments inside the mounted tool's arguments field; MCPX consumes remote_session_id/purpose and forwards only arguments upstream.",
		},
	}
	if upstream.OutputSchema != nil {
		result["output_schema"] = normalizeJSONValue(upstream.OutputSchema)
	}
	if upstream.Annotations != nil {
		result["annotations"] = normalizeJSONValue(upstream.Annotations)
	}
	return result
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
