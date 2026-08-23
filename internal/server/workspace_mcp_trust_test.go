package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

func TestWorkspaceMCPTrustApprovalPersistsAndEnablesInstructions(t *testing.T) {
	rt, workspace, remoteID, script := newWorkspaceMCPTrustRuntime(t)

	openedInstructions := instructionDocumentsFromSession(t, rt, remoteID)
	if hasInstructionID(openedInstructions, "mcp:local") {
		t.Fatalf("unapproved Workspace MCP instructions leaked into Session: %+v", openedInstructions)
	}

	servers := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID,
	})
	if !statusOK(servers) {
		t.Fatalf("mcp list=%+v", servers)
	}
	items := asMapSlice(servers["data"].(map[string]any)["servers"])
	if len(items) != 1 || items[0]["name"] != "local" || items[0]["source"] != "workspace" || items[0]["enabled"] != true || items[0]["trusted"] != false || items[0]["trust_requested"] != true || items[0]["trust_state"] != "needs_approval" {
		t.Fatalf("Workspace trust inventory=%+v", items)
	}

	callRequest := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "invoke trusted Workspace MCP",
		"server": "local", "tool": "echo", "arguments": map[string]any{"value": "one"},
	}
	waiting := callEnvelope(t, rt.toolMCPTool, context.Background(), callRequest)
	if waiting["status"] != "waiting_confirmation" || errorCode(waiting) != "mcp_trust_confirmation_required" {
		t.Fatalf("first Workspace trust request=%+v", waiting)
	}

	confirmedRequest := cloneMap(callRequest)
	confirmedRequest["user_confirmed"] = true
	confirmed, err := rt.toolMCPTool(context.Background(), mcpresult.Request(confirmedRequest))
	if err != nil {
		t.Fatal(err)
	}
	if confirmed == nil || confirmed.IsError || mcpresult.FirstText(confirmed) != "workspace:one" {
		t.Fatalf("confirmed Workspace trust call=%+v", confirmed)
	}

	manager, err := rt.mcpManagerForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	trusted, ok := manager.ServerConfig("local")
	if !ok || !trusted.Trust {
		t.Fatalf("approved Workspace registration did not become trusted: %+v ok=%v", trusted, ok)
	}

	read := callEnvelope(t, rt.toolRuntimeRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "instructions", "id": "mcp:local",
	})
	if !statusOK(read) || read["data"].(map[string]any)["content"] != "workspace trust instructions" {
		t.Fatalf("approved Workspace instructions=%+v", read)
	}

	directRequest := cloneMap(callRequest)
	directRequest["arguments"] = map[string]any{"value": "two"}
	direct, err := rt.toolMCPTool(context.Background(), mcpresult.Request(directRequest))
	if err != nil {
		t.Fatal(err)
	}
	if direct == nil || direct.IsError || mcpresult.FirstText(direct) != "workspace:two" {
		t.Fatalf("persistently trusted Workspace call=%+v", direct)
	}

	merged, err := config.LoadMergedMCP(workspace)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := merged.MCPServers["local"].TrustFingerprint
	approved, err := rt.mcpTrust.IsApproved(workspace, "local", fingerprint)
	if err != nil || !approved {
		t.Fatalf("durable Workspace trust approval=%v err=%v", approved, err)
	}

	_ = script
}

func TestWorkspaceMCPEnabledDoesNotInvalidateTrustButArgsChangeDoes(t *testing.T) {
	rt, workspace, remoteID, script := newWorkspaceMCPTrustRuntime(t)
	approveWorkspaceMCPTrust(t, rt, remoteID)

	disabled := false
	writeWorkspaceMCPTrustConfig(t, workspace, script, []string{script}, &disabled, true)
	servers := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{"action": "list", "remote_session_id": remoteID})
	items := asMapSlice(servers["data"].(map[string]any)["servers"])
	if len(items) != 1 || items[0]["state"] != "disabled" || items[0]["enabled"] != false || items[0]["trust_state"] != "trusted" {
		t.Fatalf("disabled Workspace MCP inventory=%+v", items)
	}
	disabledCall := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "verify disabled registration",
		"server": "local", "tool": "echo", "arguments": map[string]any{"value": "disabled"},
	})
	if statusOK(disabledCall) || errorCode(disabledCall) != "mcp_server_disabled" {
		t.Fatalf("disabled Workspace MCP call=%+v", disabledCall)
	}

	writeWorkspaceMCPTrustConfig(t, workspace, script, []string{script}, nil, true)
	reenabled, err := rt.toolMCPTool(context.Background(), mcpresult.Request(map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "verify re-enabled trusted registration",
		"server": "local", "tool": "echo", "arguments": map[string]any{"value": "reenabled"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if reenabled == nil || reenabled.IsError || mcpresult.FirstText(reenabled) != "workspace:reenabled" {
		t.Fatalf("re-enabled registration should reuse trust approval: %+v", reenabled)
	}

	writeWorkspaceMCPTrustConfig(t, workspace, script, []string{script, "changed"}, nil, true)
	servers = callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{"action": "list", "remote_session_id": remoteID})
	items = asMapSlice(servers["data"].(map[string]any)["servers"])
	if len(items) != 1 || items[0]["trust_state"] != "needs_approval" || items[0]["trusted"] != false {
		t.Fatalf("changed registration should lose effective trust: %+v", items)
	}
	changedCall := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "verify changed registration trust",
		"server": "local", "tool": "echo", "arguments": map[string]any{"value": "changed"},
	})
	if changedCall["status"] != "waiting_confirmation" || errorCode(changedCall) != "mcp_trust_confirmation_required" {
		t.Fatalf("changed registration should require trust reapproval: %+v", changedCall)
	}
	instructions := callEnvelope(t, rt.toolRuntimeRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "instructions",
	})
	if !statusOK(instructions) {
		t.Fatalf("instruction list after trust invalidation=%+v", instructions)
	}
	if hasInstructionID(asMapSlice(instructions["data"].(map[string]any)["documents"]), "mcp:local") {
		t.Fatalf("unapproved changed MCP instructions leaked: %+v", instructions)
	}
}

func newWorkspaceMCPTrustRuntime(t *testing.T) (*Runtime, string, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := filepath.Join(home, "project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "project", Path: workspace}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	script := writeWorkspaceTrustMCPServer(t, home)
	writeWorkspaceMCPTrustConfig(t, workspace, script, []string{script}, nil, true)
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "project"})
	if !statusOK(opened) {
		t.Fatalf("open Session=%+v", opened)
	}
	return rt, workspace, opened["remote_session_id"].(string), script
}

func writeWorkspaceMCPTrustConfig(t *testing.T, workspace, script string, args []string, enabled *bool, inject bool) {
	t.Helper()
	serverArgs := append([]string(nil), args...)
	if len(serverArgs) == 0 {
		serverArgs = []string{script}
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspace), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"local": {
			Command: "python3", Args: serverArgs, Enabled: enabled, Trust: true, InjectInstructions: inject,
			Description: "Workspace trust test", Env: map[string]string{"IGNORED_FOR_TRUST": "one"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
}

func approveWorkspaceMCPTrust(t *testing.T, rt *Runtime, remoteID string) {
	t.Helper()
	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "approve Workspace MCP trust",
		"server": "local", "tool": "echo", "arguments": map[string]any{"value": "approve"},
	}
	waiting := callEnvelope(t, rt.toolMCPTool, context.Background(), request)
	if waiting["status"] != "waiting_confirmation" || errorCode(waiting) != "mcp_trust_confirmation_required" {
		t.Fatalf("trust confirmation=%+v", waiting)
	}
	confirmed := cloneMap(request)
	confirmed["user_confirmed"] = true
	result, err := rt.toolMCPTool(context.Background(), mcpresult.Request(confirmed))
	if err != nil || result == nil || result.IsError || mcpresult.FirstText(result) != "workspace:approve" {
		t.Fatalf("trust approval result=%+v err=%v", result, err)
	}
}

func instructionDocumentsFromSession(t *testing.T, rt *Runtime, remoteID string) []map[string]any {
	t.Helper()
	resumed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": remoteID})
	if !statusOK(resumed) {
		t.Fatalf("resume Session=%+v", resumed)
	}
	instructions := resumed["data"].(map[string]any)["instructions"].(map[string]any)
	return asMapSlice(instructions["documents"])
}

func hasInstructionID(items []map[string]any, id string) bool {
	for _, item := range items {
		if item["id"] == id {
			return true
		}
	}
	return false
}

func writeWorkspaceTrustMCPServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "workspace-trust-server.py")
	serverCode := `#!/usr/bin/env python3
import json
import sys

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

tool = {"name": "echo", "description": "echo a value", "inputSchema": {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"], "additionalProperties": False}}

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
        send({"jsonrpc": "2.0", "id": request_id, "result": {"protocolVersion": "2025-11-25", "capabilities": {"tools": {}}, "serverInfo": {"name": "workspace-trust", "version": "1"}, "instructions": "workspace trust instructions"}})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": request_id, "result": {"tools": [tool]}})
    elif method == "tools/call":
        params = request.get("params", {})
        value = params.get("arguments", {}).get("value", "")
        send({"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": "workspace:" + value}], "isError": False}})
    else:
        send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "method not found"}})
`
	if err := os.WriteFile(path, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
