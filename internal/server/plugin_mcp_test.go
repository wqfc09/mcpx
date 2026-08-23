package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

func TestPluginToolStableSurfaceAndInboxIsolation(t *testing.T) {
	rt := newPluginV1Runtime(t)
	for name := range rt.listedToolMap() {
		if strings.HasPrefix(name, "plugin.") {
			t.Fatalf("dynamic Plugin tool leaked into stable MCPX catalog: %s", name)
		}
	}
	if rt.listedToolMap()["plugin_tool"].Name != "plugin_tool" {
		t.Fatal("stable plugin_tool is missing from MCPX catalog")
	}

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
	if len(plugins) != 3 || plugins[0]["name"] != "bad" || plugins[1]["name"] != "good" || plugins[2]["name"] != "off" {
		t.Fatalf("Plugin inventory=%+v", plugins)
	}
	if plugins[0]["installed"] != true || plugins[0]["active"] != true || plugins[1]["active"] != true || plugins[2]["active"] != false || plugins[2]["state"] != "inactive" {
		t.Fatalf("Plugin installed/active/runtime states=%+v", plugins)
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
		if !strings.EqualFold(errorCode(blocked), "MCP_PLUGIN_SURFACE_REQUIRED") || !strings.Contains(pluginTestErrorMessage(blocked), "plugin_tool") {
			t.Fatalf("mcp_tool(%s) did not point to Plugin surface: %+v", action, blocked)
		}
	}

	list := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "plugin": "good",
	})
	if !statusOK(list) {
		t.Fatalf("plugin_tool list=%+v", list)
	}
	listed := asMapSlice(list["data"].(map[string]any)["tools"])
	if len(listed) != 1 || listed[0]["name"] != "echo" || listed[0]["state"] != "available" || listed[0]["revision"] == "" {
		t.Fatalf("plugin_tool runtime tool list=%+v", listed)
	}

	described := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "plugin": "good", "tool": "echo",
	})
	if !statusOK(described) {
		t.Fatalf("plugin_tool describe=%+v", described)
	}
	describedData := described["data"].(map[string]any)
	if describedData["plugin"] != "good" || describedData["tool"] != "echo" || describedData["revision"] == "" || describedData["input_schema"] == nil || describedData["output_schema"] == nil || describedData["risk"] == nil {
		t.Fatalf("Plugin descriptor incomplete: %+v", describedData)
	}

	request := mcpresult.Request(map[string]any{
		"action":            "call",
		"remote_session_id": remoteID,
		"purpose":           "invoke Plugin echo through stable plugin_tool",
		"idempotency_key":   "plugin-echo-1",
		"plugin":            "good",
		"tool":              "echo",
		"arguments":         map[string]any{"value": "ok"},
	})
	request.Params.Name = "plugin_tool"
	result, err := rt.toolPluginTool(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || mcpresult.FirstText(result) != "plugin:ok" {
		t.Fatalf("plugin_tool call=%+v", result)
	}
	if structured, _ := result.StructuredContent.(map[string]any); structured["value"] != "ok" {
		t.Fatalf("Plugin structuredContent changed: %+v", result.StructuredContent)
	}

	privateCall := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "verify allowlist", "plugin": "good", "tool": "inbox", "arguments": map[string]any{},
	})
	if !strings.EqualFold(errorCode(privateCall), "PLUGIN_TOOL_NOT_ALLOWED") {
		t.Fatalf("private inbox must not be callable through plugin_tool call: %+v", privateCall)
	}

	inbox := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "inbox", "remote_session_id": remoteID, "purpose": "collect Plugin inboxes", "limit": 10, "wait_ms": 0,
	})
	if !statusOK(inbox) {
		t.Fatalf("plugin_tool inbox=%+v", inbox)
	}
	inboxData := inbox["data"].(map[string]any)
	items := asMapSlice(inboxData["items"])
	if len(items) != 2 || items[0]["plugin"] != "bad" || items[0]["kind"] != "plugin_inbox_failed" || items[0]["delivery"] != "immediate" || items[1]["plugin"] != "good" || items[1]["kind"] != "dependency_ready" {
		t.Fatalf("Plugin Inbox V3 failure isolation=%+v", items)
	}
	if inboxData["schema_version"] != float64(3) || inboxData["succeeded"] != float64(1) || inboxData["failed"] != float64(1) || inboxData["next_cursor"] == "" {
		t.Fatalf("Plugin Inbox aggregate metadata=%+v", inboxData)
	}
	sources := asMapSlice(inboxData["sources"])
	var goodSource map[string]any
	for _, sourceStatus := range sources {
		if sourceStatus["plugin"] == "good" {
			goodSource, _ = sourceStatus["source"].(map[string]any)
		}
	}
	if goodSource["kind"] != "mcpx_plugin_inbox" || goodSource["remote_session_id"] != remoteID || goodSource["plugin"] != "good" {
		t.Fatalf("Plugin Inbox source metadata was not injected: %+v", sources)
	}

	started := time.Now()
	quiet := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "inbox", "remote_session_id": remoteID, "purpose": "verify failed Plugin source does not hot-loop", "limit": 10, "wait_ms": 40,
	})
	if !statusOK(quiet) {
		t.Fatalf("quiet plugin_tool inbox=%+v", quiet)
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
		t.Fatalf("failed Plugin source collapsed the global wait window: elapsed=%s", elapsed)
	}
	quietData := quiet["data"].(map[string]any)
	if quietData["timed_out"] != true || quietData["failed"] != float64(1) {
		t.Fatalf("quiet Plugin Inbox metadata=%+v", quietData)
	}
	quietItems := asMapSlice(quietData["items"])
	if len(quietItems) != 1 || quietItems[0]["plugin"] != "good" || quietItems[0]["kind"] != "dependency_ready" {
		t.Fatalf("steady Plugin failure should stay in sources without repeating an immediate item: %+v", quietItems)
	}

	tooLarge := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "inbox", "remote_session_id": remoteID, "purpose": "verify shared Plugin inbox limit", "limit": 501, "wait_ms": 0,
	})
	if !strings.EqualFold(errorCode(tooLarge), "PLUGIN_INBOX_ARGUMENT_INVALID") || !strings.Contains(pluginTestErrorMessage(tooLarge), "500") {
		t.Fatalf("Plugin Inbox must reject limits above the shared private contract: %+v", tooLarge)
	}
}

func TestPluginInboxFrameworkHealthIsEdgeTriggered(t *testing.T) {
	rt := &Runtime{}
	failed := rt.markPluginInboxHealth("workspace-a", pluginInboxItem{Plugin: "Demo", Status: "failed", Error: "down"})
	if failed.HealthTransition != "failed" || !pluginInboxItemWakes(failed) || len(pluginInboxCanonicalItems(failed)) != 1 {
		t.Fatalf("initial failure edge=%+v canonical=%+v", failed, pluginInboxCanonicalItems(failed))
	}
	repeated := rt.markPluginInboxHealth("workspace-a", pluginInboxItem{Plugin: "Demo", Status: "failed", Error: "still down"})
	if repeated.HealthTransition != "" || pluginInboxItemWakes(repeated) || len(pluginInboxCanonicalItems(repeated)) != 0 {
		t.Fatalf("steady failure must not wake: %+v canonical=%+v", repeated, pluginInboxCanonicalItems(repeated))
	}
	recovered := rt.markPluginInboxHealth("workspace-a", pluginInboxItem{Plugin: "Demo", Status: "succeeded", Result: map[string]any{"items": []any{}}})
	recoveryItems := pluginInboxCanonicalItems(recovered)
	if recovered.HealthTransition != "recovered" || !pluginInboxItemWakes(recovered) || len(recoveryItems) != 1 || recoveryItems[0]["kind"] != "plugin_inbox_recovered" {
		t.Fatalf("recovery edge=%+v canonical=%+v", recovered, recoveryItems)
	}
	healthy := rt.markPluginInboxHealth("workspace-a", pluginInboxItem{Plugin: "Demo", Status: "succeeded", Result: map[string]any{"items": []any{}}})
	if healthy.HealthTransition != "" || pluginInboxItemWakes(healthy) {
		t.Fatalf("steady healthy source must stay quiet: %+v", healthy)
	}
}

func TestPluginToolSchemaDriftRecoversWithoutRuntimeRestart(t *testing.T) {
	rt, schemaFile := newMutablePluginRuntime(t)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("open Plugin session=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)

	first := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "plugin": "mutable", "tool": "echo",
	})
	if !statusOK(first) {
		t.Fatalf("initial describe=%+v", first)
	}
	firstRevision := first["data"].(map[string]any)["revision"]

	if err := os.WriteFile(schemaFile, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "detect schema drift", "plugin": "mutable", "tool": "echo", "arguments": map[string]any{"value": "old"},
	})
	if !strings.EqualFold(errorCode(stale), "PLUGIN_TOOL_SCHEMA_CHANGED") {
		t.Fatalf("schema drift should require re-describe, not MCPX restart: %+v", stale)
	}

	second := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "plugin": "mutable", "tool": "echo",
	})
	if !statusOK(second) {
		t.Fatalf("second describe=%+v", second)
	}
	secondRevision := second["data"].(map[string]any)["revision"]
	if firstRevision == secondRevision {
		t.Fatalf("schema revision did not change: %v", firstRevision)
	}

	request := mcpresult.Request(map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call new schema", "plugin": "mutable", "tool": "echo", "arguments": map[string]any{"message": "new"},
	})
	request.Params.Name = "plugin_tool"
	result, err := rt.toolPluginTool(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || mcpresult.FirstText(result) != "plugin:new" {
		t.Fatalf("call after re-describe must use new schema without restarting MCPX: %+v", result)
	}
}

func newPluginV1Runtime(t *testing.T) *Runtime {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
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

func newMutablePluginRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	workspacePath := filepath.Join(home, "demo")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	schemaFile := filepath.Join(home, "schema-mode")
	if err := os.WriteFile(schemaFile, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := writeMutablePluginTestServer(t, home)
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	cfg.Logging.Enabled = false
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"mutable": {
			Command: "python3", Args: []string{script}, Env: map[string]string{"SCHEMA_FILE": schemaFile}, IsPlugin: true, Trust: true,
			Plugin: &config.MCPPlugin{Tools: []string{"echo"}, Inbox: "inbox"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt, schemaFile
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
            event = {"id": "evt-" + NAME, "seq": 1, "created_at": "2026-08-18T00:00:00Z", "kind": "dependency_ready", "delivery": "deferred", "action_required": False, "summary": NAME + " ready"}
            send({"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": "inbox:" + NAME}], "structuredContent": {"schema_version": 3, "items": [event], "timed_out": False, "next_cursor": NAME + "-next", "source": source}, "isError": INBOX_ERROR}})
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

func writeMutablePluginTestServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "mutable.py")
	serverCode := `#!/usr/bin/env python3
import json
import os
import sys

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

def mode():
    with open(os.environ["SCHEMA_FILE"], "r", encoding="utf-8") as handle:
        return handle.read().strip()

def tools():
    field = "value" if mode() == "v1" else "message"
    return [
        {"name":"echo","description":"mutable echo","inputSchema":{"type":"object","properties":{field:{"type":"string"}},"required":[field],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}},
        {"name":"inbox","description":"inbox","inputSchema":{"type":"object","properties":{"limit":{"type":"integer"},"wait_ms":{"type":"integer"},"cursor":{"type":"string"}},"required":["limit","wait_ms"],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}}
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
        send({"jsonrpc":"2.0","id":request_id,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"mutable","version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":request_id,"result":{"tools":tools()}})
    elif method == "tools/call":
        params = request.get("params", {})
        args = params.get("arguments", {})
        if params.get("name") == "echo":
            field = "value" if mode() == "v1" else "message"
            value = args.get(field, "")
            send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"plugin:"+value}],"structuredContent":{"value":value},"isError":False}})
        elif params.get("name") == "inbox":
            send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"inbox"}],"structuredContent":{"items":[],"next_cursor":""},"isError":False}})
        else:
            send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32602,"message":"unknown tool"}})
    else:
        send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32601,"message":"method not found"}})
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
