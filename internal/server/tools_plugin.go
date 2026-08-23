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
	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

const pluginCursorPrefix = "v1:"

func (r *Runtime) toolPluginTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	switch action := toolAction(req); action {
	case "list":
		return r.pluginToolList(ctx, req)
	case "describe":
		return r.pluginToolDescribe(ctx, req)
	case "call":
		return r.pluginToolCall(ctx, req)
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
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"plugins": r.pluginInventory(ws, "", ctx)})
	}
	mount, ok, err := r.pluginMountForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	server, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"plugin": pluginName, "runtime": mount.Server.Plugin.RuntimeType(), "tools": []map[string]any{}})
	}
	prepared, err := r.prepareMCPPluginServer(ws, pluginName, server)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	lease, _, err := r.pluginLeases.Acquire(ctx, lifecycleSessionHolderID(session.ID), pluginName, prepared, ws)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_UNAVAILABLE", err.Error())
	}
	currentTools, err := lease.Client.ListTools(ctx)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_UNAVAILABLE", err.Error())
	}
	current := make(map[string]*mcp.Tool, len(currentTools))
	for _, tool := range currentTools {
		if tool != nil {
			current[tool.Name] = tool
		}
	}
	allowed := pluginAllowedToolNames(mount.Server.Plugin)
	items := make([]map[string]any, 0, len(allowed))
	for _, name := range allowed {
		item := map[string]any{"name": name, "state": "missing"}
		if tool := current[name]; tool != nil {
			item["state"] = "available"
			item["description"] = tool.Description
			item["revision"] = mcpRevision([]*mcp.Tool{tool})
		}
		items = append(items, item)
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"plugin": pluginName, "runtime": "mcp", "tools": items})
}

func (r *Runtime) pluginToolDescribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	pluginName := strings.TrimSpace(stringPayload(envReq.Payload, "plugin"))
	mount, ok, err := r.pluginMountForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	server, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_TOOL_UNSUPPORTED", fmt.Sprintf("Plugin %q does not expose MCP tools", pluginName))
	}
	toolName := strings.TrimSpace(stringPayload(envReq.Payload, "tool"))
	if !pluginAllowsTool(mount.Server.Plugin, toolName) {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_TOOL_NOT_ALLOWED", fmt.Sprintf("Plugin %q tool %q is not in the effective plugin.tools allowlist", pluginName, toolName))
	}
	ws := r.workspaceRuntime(session.WorkspaceName, session.WorkspacePath)
	prepared, err := r.prepareMCPPluginServer(ws, pluginName, server)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	lease, _, err := r.pluginLeases.Acquire(ctx, lifecycleSessionHolderID(session.ID), pluginName, prepared, ws)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_UNAVAILABLE", err.Error())
	}
	tools, err := lease.Client.ListTools(ctx)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_UNAVAILABLE", err.Error())
	}
	upstream, ok := mcpToolForLease(tools, toolName)
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_TOOL_NOT_FOUND", fmt.Sprintf("Plugin %q current runtime did not return allowlisted tool %q", pluginName, toolName))
	}
	descriptor := pluginToolDescriptor(pluginName, server, upstream)
	revision, _ := descriptor["revision"].(string)
	r.upsertDiscoveryLease(discoveryLease{
		Revision: revision, RemoteSessionID: session.ID, PrincipalID: principal.ID,
		WorkspacePath: session.WorkspacePath, Kind: "plugin", Object: pluginName + "/" + toolName,
	})
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, descriptor)
}

func (r *Runtime) pluginToolCall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	pluginName := strings.TrimSpace(stringPayload(envReq.Payload, "plugin"))
	toolName := strings.TrimSpace(stringPayload(envReq.Payload, "tool"))
	mount, ok, err := r.pluginMountForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_TOOL_UNSUPPORTED", fmt.Sprintf("Plugin %q does not expose MCP tools", pluginName))
	}
	if !pluginAllowsTool(mount.Server.Plugin, toolName) {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_TOOL_NOT_ALLOWED", fmt.Sprintf("Plugin %q tool %q is not in the effective plugin.tools allowlist", pluginName, toolName))
	}
	server, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	ws := r.workspaceRuntime(session.WorkspaceName, session.WorkspacePath)
	prepared, err := r.prepareMCPPluginServer(ws, pluginName, server)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	lease, _, err := r.pluginLeases.Acquire(ctx, lifecycleSessionHolderID(session.ID), pluginName, prepared, ws)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_UNAVAILABLE", err.Error())
	}
	arguments, argumentsOK := envReq.Payload["arguments"].(map[string]any)
	if raw, exists := envReq.Payload["arguments"]; exists && raw != nil && !argumentsOK {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_ARGUMENT_INVALID", "arguments must be an object")
	}
	syntheticArguments := map[string]any{
		"action": "call", "remote_session_id": session.ID, "purpose": firstSemanticPurpose(envReq),
		"server": pluginName, "tool": toolName, "arguments": arguments,
	}
	if confirmed, ok := envReq.Payload["user_confirmed"].(bool); ok {
		syntheticArguments["user_confirmed"] = confirmed
	}
	if key := strings.TrimSpace(stringPayload(envReq.Payload, "idempotency_key")); key != "" {
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
	return r.mcpToolCallWithExistingClient(ctx, synthetic, "plugin_tool", lease.Client, lease.Server)
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
	mount, ok, err := r.pluginMountForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NOT_FOUND", fmt.Sprintf("Plugin %q is not registered", pluginName))
	}
	if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeNative {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_SIGNAL_UNSUPPORTED", fmt.Sprintf("Plugin %q is not a Native runtime", pluginName))
	}
	server, active, err := r.effectivePluginForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_CONFIG_ERROR", err.Error())
	}
	if !active {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_DISABLED", fmt.Sprintf("Plugin %q is not enabled for Workspace %q", pluginName, session.WorkspaceName))
	}
	ws := r.workspaceRuntime(session.WorkspaceName, session.WorkspacePath)
	lease, err := r.controllerLeases.Acquire(ctx, lifecycleSessionHolderID(session.ID), pluginName, server, ws)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NATIVE_UNAVAILABLE", err.Error())
	}
	// A Native runtime may have restarted after Session open; binding the current
	// validated Session here restores the Host-owned identity before signaling.
	r.controllerLeases.AttachSession(session.ID, ws.ID, session.WorkspaceName)
	if !lease.hasSession(session.ID) {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NATIVE_SESSION_UNATTACHED", "Native Plugin did not attach the current Remote Session")
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
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_NATIVE_SIGNAL_FAILED", err.Error())
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{
		"plugin": pluginName, "signal": signal, "accepted": true,
	})
}

type pluginInboxItem struct {
	Plugin           string `json:"plugin"`
	Status           string `json:"status"`
	Result           any    `json:"result,omitempty"`
	Error            string `json:"error,omitempty"`
	NextCursor       string `json:"-"`
	HealthTransition string `json:"-"`
}

func (r *Runtime) markPluginInboxHealth(workspaceID string, item pluginInboxItem) pluginInboxItem {
	key := strings.TrimSpace(workspaceID) + "\x00" + item.Plugin
	failed := item.Status != "succeeded"
	r.pluginInboxHealthMu.Lock()
	if r.pluginInboxFailed == nil {
		r.pluginInboxFailed = map[string]bool{}
	}
	wasFailed := r.pluginInboxFailed[key]
	switch {
	case failed && !wasFailed:
		item.HealthTransition = "failed"
		r.pluginInboxFailed[key] = true
	case !failed && wasFailed:
		item.HealthTransition = "recovered"
		delete(r.pluginInboxFailed, key)
	}
	r.pluginInboxHealthMu.Unlock()
	return item
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
	if limit < 1 || limit > 500 {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_INBOX_ARGUMENT_INVALID", "limit must be between 1 and 500")
	}
	waitMS := intPayload(envReq.Payload, "wait_ms")
	if _, explicit := envReq.Payload["wait_ms"]; !explicit {
		waitMS = 25000
	}
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
	started := time.Now()
	if len(names) == 0 {
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{
			"schema_version": 3, "items": []any{}, "sources": []any{},
			"succeeded": 0, "failed": 0, "timed_out": false, "waited_ms": int64(0), "next_cursor": "",
		})
	}

	readAll := func(readCtx context.Context, sourceWaitMS int) []pluginInboxItem {
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
				items[index] = r.readPluginInbox(readCtx, envReq, principal.ID, session, name, pluginCursor, limit, sourceWaitMS)
			}()
		}
		fanout.Wait()
		return items
	}

	items := []pluginInboxItem{}
	wokeEarly := false
	if waitMS == 0 {
		items = readAll(ctx, 0)
		for index := range items {
			items[index] = r.markPluginInboxHealth(session.WorkspaceID, items[index])
		}
	} else {
		waitCtx, cancel := context.WithCancel(ctx)
		type sourceResult struct {
			index int
			item  pluginInboxItem
		}
		results := make(chan sourceResult, len(names))
		for index, name := range names {
			index, name := index, name
			go func() {
				pluginCursor := cursors[name]
				if pluginCursor == "" {
					pluginCursor = cursors["*"]
				}
				results <- sourceResult{index: index, item: r.readPluginInbox(waitCtx, envReq, principal.ID, session, name, pluginCursor, limit, waitMS)}
			}()
		}
		collected := make([]pluginInboxItem, len(names))
		healthTransitions := map[string]string{}
		rememberTransition := func(item pluginInboxItem) {
			if item.HealthTransition != "" {
				healthTransitions[item.Plugin] = item.HealthTransition
			}
		}
		annotateSnapshot := func(snapshot []pluginInboxItem) bool {
			wakes := false
			for index := range snapshot {
				snapshot[index] = r.markPluginInboxHealth(session.WorkspaceID, snapshot[index])
				rememberTransition(snapshot[index])
				if transition := healthTransitions[snapshot[index].Plugin]; transition != "" {
					snapshot[index].HealthTransition = transition
				}
				if pluginInboxItemWakes(snapshot[index]) {
					wakes = true
				}
			}
			return wakes
		}
		for remaining := len(names); remaining > 0; remaining-- {
			result := <-results
			if !wokeEarly {
				result.item = r.markPluginInboxHealth(session.WorkspaceID, result.item)
				rememberTransition(result.item)
			}
			collected[result.index] = result.item
			if !wokeEarly && pluginInboxItemWakes(result.item) {
				wokeEarly = true
				cancel()
			}
		}
		cancel()
		if wokeEarly && ctx.Err() == nil {
			// Re-read all sources from the original cursor after canceling their
			// long polls. This captures deferred events that arrived in the same
			// window and replaces cancellation errors with a stable snapshot.
			items = readAll(ctx, 0)
			annotateSnapshot(items)
		} else {
			items = collected
			// A failed source may return immediately instead of honoring wait_ms.
			// Framework health failures are reported in the final snapshot, but
			// they must not collapse the global attention window into a hot loop.
			remainingWait := time.Duration(waitMS)*time.Millisecond - time.Since(started)
			if remainingWait > 0 && ctx.Err() == nil {
				select {
				case <-time.After(remainingWait):
					items = readAll(ctx, 0)
					if annotateSnapshot(items) {
						wokeEarly = true
					}
				case <-ctx.Done():
				}
			}
		}
	}

	next := map[string]string{}
	for _, name := range names {
		if cursor := cursors[name]; cursor != "" {
			next[name] = cursor
		} else if cursor := cursors["*"]; cursor != "" {
			next[name] = cursor
		}
	}
	canonical := make([]map[string]any, 0)
	taxonomies := make([]map[string]any, 0)
	sources := make([]map[string]any, 0, len(items))
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
		canonical = append(canonical, pluginInboxCanonicalItems(item)...)
		taxonomies = append(taxonomies, pluginInboxTaxonomies(item)...)
		source := map[string]any{"plugin": item.Plugin, "status": item.Status}
		if item.Error != "" {
			source["error"] = item.Error
		}
		if data := pluginInboxItemData(item); data != nil {
			if timedOut, ok := data["timed_out"].(bool); ok {
				source["timed_out"] = timedOut
			}
			if upstreamSource, ok := data["source"].(map[string]any); ok {
				source["source"] = upstreamSource
			}
		}
		sources = append(sources, source)
	}
	sort.SliceStable(canonical, func(i, j int) bool {
		leftTime, _ := canonical[i]["created_at"].(string)
		rightTime, _ := canonical[j]["created_at"].(string)
		if leftTime != rightTime {
			return leftTime < rightTime
		}
		leftPlugin, _ := canonical[i]["plugin"].(string)
		rightPlugin, _ := canonical[j]["plugin"].(string)
		if leftPlugin != rightPlugin {
			return leftPlugin < rightPlugin
		}
		return fmt.Sprint(canonical[i]["seq"]) < fmt.Sprint(canonical[j]["seq"])
	})
	nextCursor, err := encodePluginCursor(next)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLUGIN_INBOX_CURSOR_ERROR", err.Error())
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{
		"schema_version": 3,
		"items":          canonical,
		"taxonomies":     taxonomies,
		"sources":        sources,
		"succeeded":      succeeded,
		"failed":         failed,
		"timed_out":      waitMS > 0 && !wokeEarly,
		"waited_ms":      time.Since(started).Milliseconds(),
		"next_cursor":    nextCursor,
	})
}

func pluginInboxItemData(item pluginInboxItem) map[string]any {
	switch result := item.Result.(type) {
	case map[string]any:
		return result
	case *mcp.CallToolResult:
		data, _ := result.StructuredContent.(map[string]any)
		return data
	default:
		return nil
	}
}

func pluginInboxTaxonomies(item pluginInboxItem) []map[string]any {
	if item.Status != "succeeded" {
		return nil
	}
	data := pluginInboxItemData(item)
	if data == nil {
		return nil
	}
	out := make([]map[string]any, 0, 1)
	appendTaxonomy := func(value any) {
		taxonomy, ok := value.(map[string]any)
		if !ok || len(taxonomy) == 0 {
			return
		}
		copy := make(map[string]any, len(taxonomy)+1)
		copy["plugin"] = item.Plugin
		for key, field := range taxonomy {
			copy[key] = field
		}
		out = append(out, copy)
	}
	appendTaxonomy(data["taxonomy"])
	if values, ok := data["taxonomies"].([]any); ok {
		for _, value := range values {
			appendTaxonomy(value)
		}
	}
	return out
}

func pluginInboxRawItems(data map[string]any) []map[string]any {
	if data == nil {
		return nil
	}
	switch raw := data["items"].(type) {
	case []map[string]any:
		return raw
	case []any:
		items := make([]map[string]any, 0, len(raw))
		for _, value := range raw {
			if item, ok := value.(map[string]any); ok {
				items = append(items, item)
			}
		}
		return items
	default:
		return nil
	}
}

func pluginInboxItemWakes(item pluginInboxItem) bool {
	if item.HealthTransition != "" {
		return true
	}
	if item.Status != "succeeded" {
		return false
	}
	data := pluginInboxItemData(item)
	if data == nil {
		return true
	}
	if hasMore, _ := data["has_more"].(bool); hasMore {
		return true
	}
	for _, event := range pluginInboxRawItems(data) {
		if delivery, _ := event["delivery"].(string); delivery == "immediate" {
			return true
		}
	}
	return false
}

func pluginInboxCanonicalItems(item pluginInboxItem) []map[string]any {
	if item.Status != "succeeded" {
		if item.HealthTransition != "failed" {
			return nil
		}
		return []map[string]any{{
			"id":              "mcpx_inbox_failure_" + item.Plugin,
			"kind":            "plugin_inbox_failed",
			"delivery":        "immediate",
			"action_required": false,
			"summary":         fmt.Sprintf("Plugin %s Inbox unavailable: %s", item.Plugin, item.Error),
			"plugin":          item.Plugin,
			"data":            map[string]any{"error": item.Error},
		}}
	}
	data := pluginInboxItemData(item)
	rawItems := pluginInboxRawItems(data)
	out := make([]map[string]any, 0, len(rawItems)+1)
	if item.HealthTransition == "recovered" {
		out = append(out, map[string]any{
			"id":              "mcpx_inbox_recovered_" + item.Plugin,
			"kind":            "plugin_inbox_recovered",
			"delivery":        "immediate",
			"action_required": false,
			"summary":         fmt.Sprintf("Plugin %s Inbox recovered", item.Plugin),
			"plugin":          item.Plugin,
		})
	}
	for _, raw := range rawItems {
		event := make(map[string]any, len(raw)+1)
		for key, value := range raw {
			event[key] = value
		}
		delivery, _ := event["delivery"].(string)
		actionRequired, _ := event["action_required"].(bool)
		if delivery == "" {
			if actionRequired {
				delivery = "immediate"
			} else {
				delivery = "deferred"
			}
			event["delivery"] = delivery
		}
		if delivery != "immediate" && delivery != "deferred" {
			continue
		}
		if actionRequired && delivery != "immediate" {
			continue
		}
		if summary, _ := event["summary"].(string); strings.TrimSpace(summary) == "" {
			if kind, _ := event["kind"].(string); strings.TrimSpace(kind) != "" {
				event["summary"] = kind
			}
		}
		event["plugin"] = item.Plugin
		out = append(out, event)
	}
	return out
}

func (r *Runtime) readPluginInbox(parent context.Context, envReq envelope.Request, principalID string, session remotesession.Session, pluginName, cursor string, limit, waitMS int) pluginInboxItem {
	item := pluginInboxItem{Plugin: pluginName, Status: "failed"}
	mount, ok, err := r.pluginMountForWorkspace(session.WorkspacePath, pluginName)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	if !ok {
		item.Error = "Plugin definition is unavailable"
		return item
	}
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
	if mount.Server.Plugin != nil && mount.Server.Plugin.RuntimeType() == config.PluginRuntimeNative {
		lease, ensureErr := r.controllerLeases.Acquire(parent, lifecycleSessionHolderID(session.ID), pluginName, server, ws)
		if ensureErr != nil {
			item.Error = ensureErr.Error()
			return item
		}
		// Inbox polling is also a natural re-attachment point after a Controller
		// process restart within a still-open Remote Session.
		r.controllerLeases.AttachSession(session.ID, ws.ID, session.WorkspaceName)
		result, nextCursor, readErr := lease.inbox.Read(parent, cursor, limit, waitMS)
		if readErr != nil {
			item.Error = readErr.Error()
			return item
		}
		item.Result = result
		item.NextCursor = nextCursor
		item.Status = "succeeded"
		return item
	}
	inboxName := ""
	if mount.Server.Plugin != nil {
		inboxName = strings.TrimSpace(mount.Server.Plugin.Inbox)
	}
	if inboxName == "" {
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
	lease, tools, err := r.pluginLeases.Acquire(ctx, lifecycleSessionHolderID(session.ID), pluginName, prepared, ws)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	client := lease.Client
	current, ok := mcpToolForLease(tools, inboxName)
	if !ok {
		item.Error = fmt.Sprintf("inbox tool %q was not returned by current tools/list", inboxName)
		return item
	}
	currentRevision := mcpRevision([]*mcp.Tool{current})
	arguments := map[string]any{"limit": limit, "wait_ms": waitMS}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	if err := validateDiscoveryArguments(discoverySchemaMap(current.InputSchema), arguments); err != nil {
		item.Error = err.Error()
		return item
	}
	risk := mcpExecutionRiskForServer(server, current)
	if confirmation := r.extensionConfirmationGate(ctx, envReq, principalID, session, "plugin_tool", pluginName+"/"+inboxName, currentRevision, risk); confirmation != nil {
		item.Error = "generic confirmation is required for this Plugin inbox"
		item.Result = confirmation
		return item
	}
	meta := mcpCallRequestMeta(envReq, session)
	meta[mcpMetaSource] = map[string]any{
		"kind": "mcpx_plugin_inbox", "plugin": pluginName, "remote_session_id": session.ID,
		"workspace_id": session.WorkspaceID, "workspace": session.WorkspaceName, "request_id": envReq.RequestID,
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
	augmentMCPCallResult(result, envReq, session, pluginName, current.Name)
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
	r.pluginMu.RLock()
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	r.pluginMu.RUnlock()
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
