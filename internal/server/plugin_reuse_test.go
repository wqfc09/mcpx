package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcpx/internal/config"
	"mcpx/internal/workspace"
)

type pluginStartRecord struct {
	PID         int    `json:"pid"`
	CWD         string `json:"cwd"`
	Workspace   string `json:"workspace"`
	WorkspaceID string `json:"workspace_id"`
	RuntimeDir  string `json:"runtime_dir"`
	InstanceID  string `json:"instance_id"`
	Catalog     bool   `json:"catalog"`
}

func TestWorkspaceScopedPluginRuntimeReusesPerWorkspaceAndIsolatesAcrossWorkspaces(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeWorkspace)
	alpha, _ := rt.reg.Get("alpha")
	beta, _ := rt.reg.Get("beta")

	// Catalog probing is one-shot and Workspace-independent. It must not consume
	// a business lease or leak Workspace launch context.
	if records := readPluginStarts(t, starts); len(records) != 1 || !records[0].Catalog || records[0].Workspace != "" || records[0].WorkspaceID != "" {
		t.Fatalf("Plugin catalog probe=%+v", records)
	}
	for i := 0; i < 2; i++ {
		opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "alpha"})
		if !statusOK(opened) {
			t.Fatalf("open alpha Session %d=%+v", i, opened)
		}
	}
	if records := businessPluginStarts(readPluginStarts(t, starts)); len(records) != 1 {
		t.Fatalf("same Workspace spawned duplicate Plugin runtimes: %+v", records)
	}

	openedBeta := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "beta"})
	if !statusOK(openedBeta) {
		t.Fatalf("open beta Session=%+v", openedBeta)
	}
	records := businessPluginStarts(readPluginStarts(t, starts))
	if len(records) != 2 {
		t.Fatalf("second Workspace must get one isolated Plugin runtime: %+v", records)
	}
	byWorkspace := map[string]pluginStartRecord{}
	for _, record := range records {
		byWorkspace[record.Workspace] = record
	}
	for _, ws := range []workspace.Workspace{alpha, beta} {
		record, ok := byWorkspace[ws.Path]
		if !ok {
			t.Fatalf("missing Plugin runtime for %s: %+v", ws.Path, records)
		}
		if record.CWD != ws.Path || record.WorkspaceID != ws.ID || record.InstanceID != rt.instanceID {
			t.Fatalf("workspace Plugin launch context=%+v want ws=%+v instance=%s", record, ws, rt.instanceID)
		}
		wantRuntime := filepath.Join(home, "runtime", "plugins", "reuse", ws.ID)
		if record.RuntimeDir != wantRuntime {
			t.Fatalf("workspace Plugin runtime_dir=%q want=%q", record.RuntimeDir, wantRuntime)
		}
	}
}

func TestInstanceScopedPluginRuntimeReusesAcrossWorkspaces(t *testing.T) {
	rt, home, starts := newPluginReuseRuntime(t, config.PluginScopeInstance)
	for _, name := range []string{"alpha", "beta", "alpha"} {
		opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": name})
		if !statusOK(opened) {
			t.Fatalf("open %s Session=%+v", name, opened)
		}
	}
	records := businessPluginStarts(readPluginStarts(t, starts))
	if len(records) != 1 {
		t.Fatalf("instance-scoped Plugin must use one process across Workspaces: %+v", records)
	}
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].Workspace != "" || records[0].WorkspaceID != "" || records[0].CWD != physicalHome {
		t.Fatalf("instance Plugin received Workspace-local launch context: %+v", records[0])
	}
	if records[0].RuntimeDir != filepath.Join(home, "runtime", "plugins", "reuse", "instance") || records[0].InstanceID != rt.instanceID {
		t.Fatalf("instance Plugin runtime context=%+v", records[0])
	}
}

func newPluginReuseRuntime(t *testing.T, scope string) (*Runtime, string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	alpha := filepath.Join(home, "alpha")
	beta := filepath.Join(home, "beta")
	for _, path := range []string{alpha, beta} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "alpha", Path: alpha}, {Name: "beta", Path: beta}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(home, "plugin-starts.jsonl")
	script := writePluginReuseServer(t, home)
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"reuse": {
			Command: "python3", Args: []string{script}, Trust: true, IsPlugin: true,
			Env: map[string]string{
				"START_LOG":        starts,
				"MCPX_INSTANCE_ID": "spoofed-instance",
				"MCPX_WORKSPACE":   "spoofed-workspace",
			},
			Plugin: &config.MCPPlugin{Scope: scope, Tools: []string{"echo"}, Inbox: "inbox"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{InstanceID: "mcpx_reuse_test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt, home, starts
}

func writePluginReuseServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "plugin-reuse.py")
	code := `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["START_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps({
        "pid": os.getpid(),
        "cwd": os.getcwd(),
        "workspace": os.environ.get("MCPX_WORKSPACE", ""),
        "workspace_id": os.environ.get("MCPX_WORKSPACE_ID", ""),
        "runtime_dir": os.environ.get("MCPX_PLUGIN_RUNTIME_DIR", ""),
        "instance_id": os.environ.get("MCPX_INSTANCE_ID", ""),
        "catalog": os.environ.get("MCPX_PLUGIN_CATALOG", "") == "1"
    }, separators=(",", ":")) + "\n")
    handle.flush()

def send(message):
    sys.stdout.write(json.dumps(message, separators=(",", ":")) + "\n")
    sys.stdout.flush()

tools = [
    {"name":"echo","description":"echo","inputSchema":{"type":"object","properties":{"value":{"type":"string"}},"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}},
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
        send({"jsonrpc":"2.0","id":request_id,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"reuse","version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":request_id,"result":{"tools":tools}})
    elif method == "tools/call":
        params = request.get("params", {})
        name = params.get("name")
        if name == "echo":
            value = params.get("arguments", {}).get("value", "")
            send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"echo:" + value}],"isError":False}})
        elif name == "inbox":
            send({"jsonrpc":"2.0","id":request_id,"result":{"content":[{"type":"text","text":"inbox"}],"structuredContent":{"next_cursor":"next"},"isError":False}})
        else:
            send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32602,"message":"unknown tool"}})
    else:
        send({"jsonrpc":"2.0","id":request_id,"error":{"code":-32601,"message":"method not found"}})
`
	if err := os.WriteFile(path, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func businessPluginStarts(records []pluginStartRecord) []pluginStartRecord {
	business := make([]pluginStartRecord, 0, len(records))
	for _, record := range records {
		if !record.Catalog {
			business = append(business, record)
		}
	}
	return business
}

func readPluginStarts(t *testing.T, path string) []pluginStartRecord {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	records := make([]pluginStartRecord, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record pluginStartRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}
