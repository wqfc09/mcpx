package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

type pluginMultiplexCall struct {
	result *mcp.CallToolResult
	err    error
}

func TestManagedMCPPluginMultiplexesInboxAndBusinessTool(t *testing.T) {
	rt, inboxSignal := newMultiplexPluginRuntime(t)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("open Plugin session=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)

	// Prime the managed lease so the concurrency assertion measures one existing
	// Plugin connection rather than process startup.
	described := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "plugin": "parallel", "tool": "echo",
	})
	if !statusOK(described) {
		t.Fatalf("describe parallel Plugin=%+v", described)
	}

	inboxDone := make(chan pluginMultiplexCall, 1)
	go func() {
		result, err := rt.toolPluginTool(context.Background(), mcpresult.Request(map[string]any{
			"action": "inbox", "remote_session_id": remoteID, "purpose": "hold private Inbox open", "limit": 10, "wait_ms": 2000,
		}))
		inboxDone <- pluginMultiplexCall{result: result, err: err}
	}()
	waitForPluginInboxSignal(t, inboxSignal)

	echoDone := make(chan pluginMultiplexCall, 1)
	go func() {
		result, err := rt.toolPluginTool(context.Background(), mcpresult.Request(map[string]any{
			"action": "call", "remote_session_id": remoteID, "purpose": "verify multiplexed Plugin Tool call",
			"plugin": "parallel", "tool": "echo", "arguments": map[string]any{"value": "fast"},
		}))
		echoDone <- pluginMultiplexCall{result: result, err: err}
	}()

	select {
	case outcome := <-echoDone:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result == nil || outcome.result.IsError || mcpresult.FirstText(outcome.result) != "plugin:fast" {
			t.Fatalf("business Tool failed while Inbox was waiting: %+v", outcome.result)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("managed MCP Plugin serialized business Tool behind a private Inbox long-poll")
	}

	select {
	case outcome := <-inboxDone:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result == nil || outcome.result.IsError {
			t.Fatalf("private Inbox failed after multiplexed Tool call: %+v", outcome.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("private Inbox did not finish after its requested wait")
	}
}

func TestManagedMCPPluginReuseRefreshesLiveTools(t *testing.T) {
	rt, schemaFile := newMutablePluginRuntime(t)
	ws, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo Workspace missing")
	}
	server, active, err := rt.effectivePluginForWorkspace(ws.Path, "mutable")
	if err != nil || !active {
		t.Fatalf("resolve mutable Plugin: active=%v err=%v", active, err)
	}

	first, tools, err := rt.pluginLeases.Ensure(context.Background(), "mutable", server, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !pluginToolHasInputProperty(tools, "echo", "value") {
		t.Fatalf("initial live tools/list did not expose v1 schema: %+v", tools)
	}
	if err := os.WriteFile(schemaFile, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, refreshed, err := rt.pluginLeases.Ensure(context.Background(), "mutable", server, ws)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("schema-only live tools/list change restarted the managed Plugin lease")
	}
	if !pluginToolHasInputProperty(refreshed, "echo", "message") || pluginToolHasInputProperty(refreshed, "echo", "value") {
		t.Fatalf("reused lease did not refresh live Tool schema: %+v", refreshed)
	}
}

func pluginToolHasInputProperty(tools []*mcp.Tool, toolName, property string) bool {
	for _, tool := range tools {
		if tool == nil || tool.Name != toolName {
			continue
		}
		schema, _ := tool.InputSchema.(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		_, ok := properties[property]
		return ok
	}
	return false
}

func newMultiplexPluginRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	workspacePath := filepath.Join(home, "demo")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	inboxSignal := filepath.Join(home, "inbox-started")
	script := writeMultiplexPluginServer(t, home)
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	cfg.Logging.Enabled = false
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"parallel": {
			Command: "python3", Args: []string{script}, Env: map[string]string{"INBOX_SIGNAL": inboxSignal}, IsPlugin: true, Trust: true,
			Plugin: &config.MCPPlugin{Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt, inboxSignal
}

func waitForPluginInboxSignal(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("private Inbox did not enter its long-poll handler")
}

func writeMultiplexPluginServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "parallel.py")
	serverCode := `#!/usr/bin/env python3
import json
import os
import sys
import threading
import time

SIGNAL = os.environ["INBOX_SIGNAL"]
WRITE_LOCK = threading.Lock()
TOOLS = [
    {"name":"echo","description":"echo","inputSchema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}},
    {"name":"inbox","description":"inbox","inputSchema":{"type":"object","properties":{"cursor":{"type":"string"},"limit":{"type":"integer"},"wait_ms":{"type":"integer"}},"required":["limit","wait_ms"],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}}
]

def send(message):
    with WRITE_LOCK:
        sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
        sys.stdout.flush()

def handle(request):
    request_id = request.get("id")
    if request_id is None:
        return
    method = request.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":request_id,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"parallel","version":"1"}}})
        return
    if method == "tools/list":
        send({"jsonrpc":"2.0","id":request_id,"result":{"tools":TOOLS}})
        return
    if method != "tools/call":
        send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32601,"message":"method not found"}})
        return
    params = request.get("params", {})
    args = params.get("arguments", {})
    if params.get("name") == "echo":
        value = args.get("value", "")
        send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"plugin:"+value}],"structuredContent":{"value":value},"isError":False}})
        return
    if params.get("name") == "inbox":
        with open(SIGNAL, "w", encoding="utf-8") as handle_file:
            handle_file.write("started")
        time.sleep(max(0, args.get("wait_ms", 0)) / 1000)
        send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"inbox"}],"structuredContent":{"schema_version":3,"items":[],"timed_out":True,"next_cursor":args.get("cursor", "")},"isError":False}})
        return
    send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32602,"message":"unknown tool"}})

for line in sys.stdin:
    try:
        request = json.loads(line)
    except Exception:
        continue
    threading.Thread(target=handle, args=(request,), daemon=True).start()
`
	if err := os.WriteFile(path, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
