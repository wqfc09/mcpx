package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

func TestPluginV1MountedToolsSurfaceAndInboxIsolation(t *testing.T) {
	rt := newPluginV1Runtime(t)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("open Plugin session=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)
	openData := opened["data"].(map[string]any)
	inventory := openData["extension_inventory"].(map[string]any)
	if servers := asMapSlice(inventory["mcp_servers"]); len(servers) != 1 || servers[0]["name"] != "ordinary" {
		t.Fatalf("Plugin leaked into ordinary MCP inventory: %+v", servers)
	}
	plugins := asMapSlice(inventory["plugins"])
	if len(plugins) != 2 || plugins[0]["name"] != "bad" || plugins[1]["name"] != "good" {
		t.Fatalf("Plugin inventory=%+v", plugins)
	}

	for _, action := range []string{"list", "describe", "call"} {
		arguments := map[string]any{"action": action, "remote_session_id": remoteID, "server": "good"}
		if action != "list" {
			arguments["tool"] = "echo"
		}
		if action == "call" {
			arguments["purpose"] = "verify Plugin isolation"
			arguments["arguments"] = map[string]any{"value": "blocked"}
		}
		blocked := callEnvelope(t, rt.toolMCPTool, context.Background(), arguments)
		if errorCode(blocked) != "mcp_plugin_surface_required" || !strings.Contains(pluginTestErrorMessage(blocked), "plugin_tool") {
			t.Fatalf("mcp_tool(%s) did not point to Plugin surface: %+v", action, blocked)
		}
	}

	publicTool, ok := rt.listedToolMap()["plugin.good.echo"]
	if !ok {
		t.Fatal("mounted Plugin tool is missing from the MCPX catalog")
	}
	expectedUpstreamInput := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []any{"value"}, "additionalProperties": false}
	mountedInput, _ := normalizeJSONValue(publicTool.InputSchema).(map[string]any)
	mountedProperties, _ := mountedInput["properties"].(map[string]any)
	if mountedInput["additionalProperties"] != false || mountedProperties["remote_session_id"] == nil || mountedProperties["purpose"] == nil || !reflect.DeepEqual(mountedProperties["arguments"], expectedUpstreamInput) {
		t.Fatalf("mounted input schema did not preserve upstream arguments inside MCPX envelope: %+v", publicTool.InputSchema)
	}
	expectedOutput := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []any{"value"}}
	if !reflect.DeepEqual(normalizeJSONValue(publicTool.OutputSchema), expectedOutput) {
		t.Fatalf("mounted output schema changed: %+v", publicTool.OutputSchema)
	}
	if publicTool.Description != "echo a value" || publicTool.Annotations == nil || !publicTool.Annotations.ReadOnlyHint {
		t.Fatalf("mounted description/annotations changed: %+v", publicTool)
	}
	if _, exposed := rt.listedToolMap()["plugin.good.inbox"]; exposed {
		t.Fatal("Plugin inbox must not be mounted as a public tool")
	}
	if _, exposed := rt.listedToolMap()["plugin.off.echo"]; exposed {
		t.Fatal("disabled Plugin must not be mounted into the process-wide tool catalog")
	}

	list := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "plugin": "good",
	})
	if !statusOK(list) || len(asMapSlice(list["data"].(map[string]any)["tools"])) != 1 {
		t.Fatalf("plugin_tool list=%+v", list)
	}
	described := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "plugin": "good", "tool": "plugin.good.echo",
	})
	if !statusOK(described) {
		t.Fatalf("plugin_tool describe=%+v", described)
	}
	describedData := described["data"].(map[string]any)
	if describedData["name"] != "plugin.good.echo" || describedData["revision"] == "" || describedData["output_schema"] == nil {
		t.Fatalf("Plugin descriptor incomplete: %+v", describedData)
	}

	request := mcpresult.Request(map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "invoke mounted Plugin echo",
		"idempotency_key":   "plugin-echo-1",
		"arguments":         map[string]any{"value": "ok"},
	})
	request.Params.Name = "plugin.good.echo"
	result, err := rt.toolHandlers["plugin.good.echo"](context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || mcpresult.FirstText(result) != "plugin:ok" {
		t.Fatalf("mounted Plugin call=%+v", result)
	}
	if structured, _ := result.StructuredContent.(map[string]any); structured["value"] != "ok" {
		t.Fatalf("mounted Plugin structuredContent changed: %+v", result.StructuredContent)
	}

	inbox := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "inbox", "remote_session_id": remoteID, "purpose": "collect Plugin inboxes", "limit": 10, "wait_ms": 0,
	})
	if !statusOK(inbox) {
		t.Fatalf("plugin_tool inbox=%+v", inbox)
	}
	inboxData := inbox["data"].(map[string]any)
	items := asMapSlice(inboxData["items"])
	if len(items) != 2 || items[0]["plugin"] != "bad" || items[0]["status"] != "failed" || items[1]["plugin"] != "good" || items[1]["status"] != "succeeded" {
		t.Fatalf("Plugin inbox failure isolation=%+v", items)
	}
	if inboxData["succeeded"] != float64(1) || inboxData["failed"] != float64(1) || inboxData["next_cursor"] == "" {
		t.Fatalf("Plugin inbox aggregate metadata=%+v", inboxData)
	}
	goodResult, _ := items[1]["result"].(map[string]any)
	goodStructured, _ := goodResult["structuredContent"].(map[string]any)
	source, _ := goodStructured["source"].(map[string]any)
	if source["kind"] != "mcpx_plugin_inbox" || source["remote_session_id"] != remoteID || source["plugin"] != "good" {
		t.Fatalf("Plugin inbox source metadata was not injected: %+v", goodStructured)
	}

}

func newPluginV1Runtime(t *testing.T) *Runtime {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspacePath := filepath.Join(home, "demo")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	goodScript := writePluginTestServer(t, home, "good.py", "good", false)
	badScript := writePluginTestServer(t, home, "bad.py", "bad", true)
	ordinaryScript := writePluginTestServer(t, home, "ordinary.py", "ordinary", false)
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	cfg.Logging.Enabled = false
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	plugin := func(script string) config.MCPServer {
		return config.MCPServer{
			Command: "python3", Args: []string{script}, IsPlugin: true, Trust: true,
			Plugin: &config.MCPPlugin{Tools: []string{"echo"}, Inbox: "inbox"},
		}
	}
	disabled := false
	off := plugin(goodScript)
	off.Enabled = &disabled
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"good":     plugin(goodScript),
		"bad":      plugin(badScript),
		"off":      off,
		"ordinary": {Command: "python3", Args: []string{ordinaryScript}},
	}}); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func writePluginTestServer(t *testing.T, dir, filename, name string, inboxError bool) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	serverCode := `#!/usr/bin/env python3
import json
import sys

NAME = ` + mustPythonString(t, name) + `
INBOX_ERROR = ` + map[bool]string{true: "True", false: "False"}[inboxError] + `

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

tools = [
    {"name": "echo", "description": "echo a value", "inputSchema": {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"], "additionalProperties": False}, "outputSchema": {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"]}, "annotations": {"readOnlyHint": True, "destructiveHint": False, "idempotentHint": True, "openWorldHint": False}},
    {"name": "inbox", "description": "read inbox", "inputSchema": {"type": "object", "properties": {"cursor": {"type": "string"}, "limit": {"type": "integer"}, "wait_ms": {"type": "integer"}}, "required": ["limit", "wait_ms"], "additionalProperties": False}, "annotations": {"readOnlyHint": True, "destructiveHint": False, "idempotentHint": True, "openWorldHint": False}}
]

for line in sys.stdin:
    try:
        request = json.loads(line)
    except Exception:
        continue
    request_id = request.get("id")
    if request_id is None:
        continue
    method = request.get("method")
    if method == "initialize":
        result = {"protocolVersion": "2025-11-25", "capabilities": {"tools": {}}, "serverInfo": {"name": NAME, "version": "1"}}
        send({"jsonrpc": "2.0", "id": request_id, "result": result})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": request_id, "result": {"tools": tools}})
    elif method == "tools/call":
        params = request.get("params", {})
        tool = params.get("name")
        args = params.get("arguments", {})
        if tool == "echo":
            value = args.get("value", "")
            send({"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": "plugin:" + value}], "structuredContent": {"value": value}, "isError": False}})
        elif tool == "inbox":
            source = params.get("_meta", {}).get("mcpx/source", {})
            send({"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": "inbox:" + NAME}], "structuredContent": {"items": [NAME], "next_cursor": NAME + "-next", "source": source}, "isError": INBOX_ERROR}})
        else:
            send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32602, "message": "unknown tool"}})
    else:
        send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "method not found"}})
`
	if err := os.WriteFile(path, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustPythonString(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func pluginTestErrorMessage(response map[string]any) string {
	errorBody, _ := response["error"].(map[string]any)
	message, _ := errorBody["message"].(string)
	return message
}
