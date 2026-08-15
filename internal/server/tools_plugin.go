package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
)

const pluginCursorPrefix = "v1:"

func (r *Runtime) toolPluginTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	switch action := toolAction(req); action {
	case "list":
		return r.pluginToolList(ctx, req)
	case "describe":
		return r.pluginToolDescribe(ctx, req)
	case "inbox":
		return r.pluginToolInbox(ctx, req)
	case "signal":
		return r.pluginToolSignal(ctx, req)
	default:
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "INVALID_ACTION", fmt.Sprintf("plugin_tool does not support action %q", action))
	}
}

func (r *Runtime) pluginToolList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	pluginName := strings.TrimSpace(stringPayload(envReq.Payload, "plugin"))
	ws := r.workspaceRuntime(session.WorkspaceName, session.WorkspacePath)
	if pluginName == "" {
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"plugins": r.pluginInventory(ws, false, ctx)})
	}
	mount, ok := r.plugins[pluginName]
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	if _, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName); err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	} else if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	toolNames := make([]string, 0, len(mount.Tools))
	for name := range mount.Tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)
	items := make([]map[string]any, 0, len(toolNames))
	for _, name := range toolNames {
		tool := mount.Tools[name]
		items = append(items, map[string]any{
			"name": mountedPluginToolName(pluginName, name), "upstream_tool": name,
			"description": tool.Description, "revision": mcpRevision([]*mcp.Tool{tool}),
		})
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"plugin": pluginName, "tools": items})
}

func (r *Runtime) pluginToolDescribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	pluginName := strings.TrimSpace(stringPayload(envReq.Payload, "plugin"))
	mount, ok := r.plugins[pluginName]
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	if _, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName); err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	} else if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	toolName := strings.TrimSpace(stringPayload(envReq.Payload, "tool"))
	prefix := mountedPluginToolName(pluginName, "")
	toolName = strings.TrimPrefix(toolName, prefix)
	descriptor := pluginToolDescriptor(mount, toolName)
	if descriptor == nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_TOOL_NOT_FOUND", fmt.Sprintf("Plugin %q has no mounted tool %q", pluginName, toolName))
	}
	revision, _ := descriptor["revision"].(string)
	r.upsertDiscoveryLease(discoveryLease{
		Revision: revision, RemoteSessionID: session.ID, PrincipalID: principal.ID,
		WorkspacePath: session.WorkspacePath, Kind: "plugin", Object: pluginName + "/" + toolName,
	})
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, descriptor)
}

func (r *Runtime) pluginToolSignal(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	pluginName := strings.TrimSpace(stringPayload(envReq.Payload, "plugin"))
	signal := strings.TrimSpace(stringPayload(envReq.Payload, "signal"))
	if pluginName == "" || signal == "" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_SIGNAL_ARGUMENT_INVALID", "plugin and signal are required")
	}
	mount, ok := r.plugins[pluginName]
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeController {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_SIGNAL_UNSUPPORTED", fmt.Sprintf("Plugin %q is not a Controller runtime", pluginName))
	}
	server, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	ws := r.workspaceRuntime(session.WorkspaceName, session.WorkspacePath)
	lease, err := r.controllerLeases.Ensure(ctx, pluginName, server, ws)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONTROLLER_UNAVAILABLE", err.Error())
	}
	// A Controller may have restarted after Session open; binding the current
	// validated Session here restores the Host-owned identity before signaling.
	r.controllerLeases.AttachSession(session.ID, ws.ID, session.WorkspaceName)
	if !lease.hasSession(session.ID) {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONTROLLER_SESSION_UNATTACHED", "Controller did not attach the current Remote Session")
	}
	data := map[string]any{}
	if raw, ok := envReq.Payload["data"].(map[string]any); ok && raw != nil {
		data = raw
	}
	if err := lease.send(map[string]any{
		"type": "event",
		"source": map[string]any{
			"kind": "owner.signal", "remote_session_id": session.ID,
			"workspace": session.WorkspaceName, "request_id": envReq.RequestID,
		},
		"event": map[string]any{
			"signal": signal, "data": data, "purpose": stringPayload(envReq.Payload, "purpose"),
		},
	}); err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONTROLLER_SIGNAL_FAILED", err.Error())
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{
		"plugin": pluginName, "signal": signal, "accepted": true,
	})
}

type pluginInboxItem struct {
	Plugin     string `json:"plugin"`
	Status     string `json:"status"`
	Result     any    `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	NextCursor string `json:"-"`
}

func (r *Runtime) pluginToolInbox(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	limit := intPayload(envReq.Payload, "limit")
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 1000 {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_INBOX_ARGUMENT_INVALID", "limit must be between 1 and 1000")
	}
	waitMS := intPayload(envReq.Payload, "wait_ms")
	if waitMS < 0 || waitMS > 60000 {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_INBOX_ARGUMENT_INVALID", "wait_ms must be between 0 and 60000")
	}
	cursors, err := decodePluginCursor(strings.TrimSpace(stringPayload(envReq.Payload, "cursor")))
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_INBOX_CURSOR_INVALID", err.Error())
	}
	names, err := r.activePluginNames(session.WorkspacePath)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	items := make([]pluginInboxItem, len(names))
	var fanout sync.WaitGroup
	for index, name := range names {
		index, name := index, name
		fanout.Add(1)
		go func() {
			defer fanout.Done()
			pluginCursor := cursors[name]
			if pluginCursor == "" {
				pluginCursor = cursors["*"]
			}
			items[index] = r.readPluginInbox(ctx, envReq, principal.ID, session, name, pluginCursor, limit, waitMS)
		}()
	}
	fanout.Wait()
	next := map[string]string{}
	succeeded, failed := 0, 0
	for _, item := range items {
		if item.Status == "succeeded" {
			succeeded++
		} else {
			failed++
		}
		if item.NextCursor != "" {
			next[item.Plugin] = item.NextCursor
		}
	}
	nextCursor, err := encodePluginCursor(next)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_INBOX_CURSOR_ERROR", err.Error())
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{
		"items": items, "succeeded": succeeded, "failed": failed, "next_cursor": nextCursor,
	})
}

func (r *Runtime) readPluginInbox(parent context.Context, envReq envelope.Request, principalID string, session remotesession.Session, pluginName, cursor string, limit, waitMS int) pluginInboxItem {
	item := pluginInboxItem{Plugin: pluginName, Status: "failed"}
	mount := r.plugins[pluginName]
	server, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	if !active {
		item.Error = "Plugin is disabled for this Workspace"
		return item
	}
	ws := r.workspaceRuntime(session.WorkspaceName, session.WorkspacePath)
	if mount.Server.Plugin != nil && mount.Server.Plugin.RuntimeType() == config.PluginRuntimeController {
		lease, ensureErr := r.controllerLeases.Ensure(parent, pluginName, server, ws)
		if ensureErr != nil {
			item.Error = ensureErr.Error()
			return item
		}
		// Inbox polling is also a natural re-attachment point after a Controller
		// process restart within a still-open Remote Session.
		r.controllerLeases.AttachSession(session.ID, ws.ID, session.WorkspaceName)
		result, nextCursor, readErr := lease.inbox.Read(cursor, limit, waitMS)
		if readErr != nil {
			item.Error = readErr.Error()
			return item
		}
		item.Result = result
		item.NextCursor = nextCursor
		item.Status = "succeeded"
		return item
	}
	if mount.Inbox == nil {
		item.Error = "configured Plugin inbox is unavailable"
		return item
	}
	timeout := 30 * time.Second
	if wait := time.Duration(waitMS)*time.Millisecond + 5*time.Second; wait > timeout {
		timeout = wait
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	prepared, err := r.prepareMCPPluginServer(ws, pluginName, server)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	lease, tools, err := r.pluginLeases.Ensure(ctx, pluginName, prepared, ws)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	client := lease.Client
	current, ok := mcpToolForLease(tools, mount.Inbox.Name)
	if !ok {
		item.Error = fmt.Sprintf("inbox tool %q was not returned by tools/list", mount.Inbox.Name)
		return item
	}
	currentRevision := mcpRevision([]*mcp.Tool{current})
	if currentRevision != mcpRevision([]*mcp.Tool{mount.Inbox}) {
		item.Error = "inbox schema changed after the MCPX Plugin catalog was built; restart MCPX"
		return item
	}
	arguments := map[string]any{"limit": limit, "wait_ms": waitMS}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	if err := validateDiscoveryArguments(discoverySchemaMap(current.InputSchema), arguments); err != nil {
		item.Error = err.Error()
		return item
	}
	risk := mcpExecutionRiskForServer(server, current)
	if confirmation := r.extensionConfirmationGate(ctx, envReq, principalID, session, "plugin_tool", pluginName+"/"+mount.Inbox.Name, currentRevision, risk); confirmation != nil {
		item.Error = "generic confirmation is required for this Plugin inbox"
		item.Result = confirmation
		return item
	}
	meta := mcpCallRequestMeta(envReq, session.ID, session.WorkspaceName)
	meta[mcpMetaSource] = map[string]any{
		"kind": "mcpx_plugin_inbox", "plugin": pluginName, "remote_session_id": session.ID,
		"workspace": session.WorkspaceName, "request_id": envReq.RequestID,
	}
	result, err := client.CallTool(ctx, current.Name, arguments, meta)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	if result == nil {
		item.Error = "upstream Plugin inbox returned no result"
		return item
	}
	if risk.ConfirmationRequired {
		contentKey := extensionConfirmationContentKey(principalID, "plugin_tool", pluginName+"/"+current.Name, currentRevision, envReq.Payload)
		r.consumeExtensionConfirmation(session.ID, principalID, "plugin_tool", contentKey)
	}
	augmentMCPCallResult(result, envReq, session.ID, session.WorkspaceName, pluginName, current.Name)
	item.Result = result
	item.NextCursor = inboxResultCursor(result)
	if result.IsError {
		item.Error = "upstream Plugin inbox returned an error result"
		return item
	}
	item.Status = "succeeded"
	return item
}

func (r *Runtime) sortedPluginNames() []string {
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func decodePluginCursor(cursor string) (map[string]string, error) {
	if cursor == "" {
		return map[string]string{}, nil
	}
	if !strings.HasPrefix(cursor, pluginCursorPrefix) {
		return map[string]string{"*": cursor}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cursor, pluginCursorPrefix))
	if err != nil {
		return nil, fmt.Errorf("invalid aggregate cursor encoding")
	}
	var decoded map[string]string
	if json.Unmarshal(raw, &decoded) != nil || decoded == nil {
		return nil, fmt.Errorf("invalid aggregate cursor payload")
	}
	return decoded, nil
}

func encodePluginCursor(cursors map[string]string) (string, error) {
	if len(cursors) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(cursors)
	if err != nil {
		return "", err
	}
	return pluginCursorPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func inboxResultCursor(result *mcp.CallToolResult) string {
	data, _ := result.StructuredContent.(map[string]any)
	for _, key := range []string{"next_cursor", "cursor"} {
		if value, _ := data[key].(string); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
