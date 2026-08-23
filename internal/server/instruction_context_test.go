package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/config"
)

func TestInstructionContextCombinesGlobalTrustedMCPAndWorkspaceInstructionsLive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := filepath.Join(home, "project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "system_prompt.md"), []byte("global instruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("workspace instruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	trustedText := filepath.Join(home, "trusted.txt")
	untrustedText := filepath.Join(home, "untrusted.txt")
	if err := os.WriteFile(trustedText, []byte("trusted instruction v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untrustedText, []byte("untrusted instruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverScript := writeInstructionTestServer(t, home)

	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "project", Path: workspace}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"trusted": {
			Command: "python3", Args: []string{serverScript, trustedText},
			Trust: true, InjectInstructions: true,
		},
		"untrusted": {
			Command: "python3", Args: []string{serverScript, untrustedText},
			InjectInstructions: true,
		},
		"no-inject": {
			Command: "python3", Args: []string{serverScript, trustedText},
			Trust: true,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	runtime, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	opened := callEnvelope(t, runtime.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "project"})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)
	openData := opened["data"].(map[string]any)
	instructionData := openData["instructions"].(map[string]any)
	documents := asMapSlice(instructionData["documents"])
	if len(documents) != 3 {
		t.Fatalf("instruction documents=%+v", documents)
	}
	wantIDs := []string{"global", "mcp:trusted", "project"}
	wantPriorities := []float64{10, 20, 30}
	for i := range wantIDs {
		if documents[i]["id"] != wantIDs[i] || documents[i]["priority"] != wantPriorities[i] {
			t.Fatalf("instruction order=%+v", documents)
		}
	}
	for _, document := range documents {
		if document["id"] == "mcp:untrusted" || document["id"] == "mcp:no-inject" {
			t.Fatalf("unauthorized MCP instructions leaked into context: %+v", documents)
		}
	}
	if openData["system_prompt"] != nil {
		t.Fatalf("Session must not expose a frozen system_prompt contract: %+v", openData["system_prompt"])
	}

	read := callEnvelope(t, runtime.toolRuntimeRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "instructions",
		"id":                "mcp:trusted",
	})
	if !statusOK(read) {
		t.Fatalf("runtime_read trusted instruction=%+v", read)
	}
	readData := read["data"].(map[string]any)
	if readData["content"] != "trusted instruction v1" {
		t.Fatalf("trusted instruction content=%q", readData["content"])
	}

	if err := os.WriteFile(trustedText, []byte("trusted instruction v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	read = callEnvelope(t, runtime.toolRuntimeRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"id":                "mcp:trusted",
	})
	if !statusOK(read) {
		t.Fatalf("runtime_read updated instruction=%+v", read)
	}
	readData = read["data"].(map[string]any)
	if readData["content"] != "trusted instruction v2" {
		t.Fatalf("MCP instructions were unexpectedly frozen: %+v", readData)
	}
}

func writeInstructionTestServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "instruction-server.py")
	serverCode := `#!/usr/bin/env python3
import json
import sys

content_path = sys.argv[1]

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    try:
        request = json.loads(line)
    except Exception:
        continue
    request_id = request.get("id")
    if request_id is None:
        continue
    if request.get("method") == "initialize":
        with open(content_path, "r", encoding="utf-8") as handle:
            instructions = handle.read()
        send({"jsonrpc": "2.0", "id": request_id, "result": {
            "protocolVersion": "2025-11-25",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "instruction-test", "version": "1"},
            "instructions": instructions
        }})
    else:
        send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "method not found"}})
`
	if err := os.WriteFile(path, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
