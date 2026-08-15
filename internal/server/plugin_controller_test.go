package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpx/internal/config"
)

func TestControllerPluginCoordinatesMountedMCPAndPinsContribution(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	home := t.TempDir()
	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "demo", Path: workspacePath}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}

	guidancePath := filepath.Join(home, "creator-review.md")
	guidance := "Review independently against the Creator contract. Report blocking findings first.\n"
	if err := os.WriteFile(guidancePath, []byte(guidance), 0o600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(workspacePath, ".mcpx", "skills", "comet-any")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: comet-any\ndescription: upstream creator\nruntime: markdown\n---\n\n# Upstream Comet Any\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contributionLog := filepath.Join(home, "contribution.json")
	mcpScript := writeControllerTargetMCP(t, home)
	controllerScript := writeControllerSidecar(t, home)
	disabled := false
	enabled := true
	global := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"Comet": {
			Command: mcpScript, Enabled: &disabled, Trust: true, IsPlugin: true,
			Env: map[string]string{"CONTRIBUTION_LOG": contributionLog},
			Plugin: &config.MCPPlugin{
				Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox",
				Accepts: []config.MCPPluginContributionSlot{{Slot: "creator.reviewer.guidance", Skill: "comet-any", MaxBytes: 1024}},
			},
		},
		"JEA": {
			Command: mcpScript, Enabled: &disabled, Trust: true, IsPlugin: true,
			Plugin: &config.MCPPlugin{
				Scope: config.PluginScopeWorkspace, Tools: []string{"echo"}, Inbox: "inbox",
			},
		},
		"Coordinator": {
			Command: controllerScript, Enabled: &disabled, Trust: true, IsPlugin: true,
			Plugin: &config.MCPPlugin{
				Runtime: config.PluginRuntimeController, Scope: config.PluginScopeWorkspace,
				Depends: []string{"Comet", "JEA"},
				Mounts: map[string]config.MCPPluginMount{
					"echo": {Plugin: "Comet", Tool: "echo", Automatic: true},
				},
				Subscriptions: []config.MCPPluginSubscription{
					{Plugin: "Comet", Kind: config.PluginSubscriptionInbox, Scope: config.PluginSubscriptionScopeWorkspace},
					{Plugin: "JEA", Kind: config.PluginSubscriptionInbox, Scope: config.PluginSubscriptionScopeSessions},
				},
				Contributes: []config.MCPPluginContribution{{Plugin: "Comet", Slot: "creator.reviewer.guidance", Path: guidancePath}},
			},
		},
	}}
	if err := config.WriteMCPFile(filepath.Join(home, ".mcp.json"), global); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.ProjectMCPPath(workspacePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspacePath), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"Comet": {Enabled: &enabled}, "JEA": {Enabled: &enabled}, "Coordinator": {Enabled: &enabled},
	}}); err != nil {
		t.Fatal(err)
	}

	rt, err := New(Options{InstanceID: "mcpx_controller_test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("open=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)
	openData := opened["data"].(map[string]any)
	inventory := openData["extension_inventory"].(map[string]any)
	foundAttachedSession := false
	for _, plugin := range asMapSlice(inventory["plugins"]) {
		if plugin["name"] != "Coordinator" {
			continue
		}
		if plugin["state"] != "running" {
			t.Fatalf("Controller did not start: %+v", plugin)
		}
		for _, attached := range plugin["attached_sessions"].([]any) {
			if attached == remoteID {
				foundAttachedSession = true
			}
		}
	}
	if !foundAttachedSession {
		t.Fatalf("Controller inventory did not attach current Remote Session: %+v", inventory)
	}

	described := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "name": "comet-any",
	})
	if !statusOK(described) {
		t.Fatalf("describe contributed Skill=%+v", described)
	}
	describedData := described["data"].(map[string]any)
	instructions, _ := describedData["instructions"].(string)
	if !strings.Contains(instructions, "# Upstream Comet Any") || !strings.Contains(instructions, guidance) || describedData["contribution_revision"] == "" {
		t.Fatalf("contributed Skill instructions=%+v", describedData)
	}

	deadline := time.Now().Add(5 * time.Second)
	var controllerEvents []controllerInboxRecord
	var controllerLease *controllerRuntimeLease
	for time.Now().Before(deadline) {
		rt.controllerLeases.mu.Lock()
		controllerLease = nil
		for _, candidate := range rt.controllerLeases.leases {
			if candidate.Plugin == "Coordinator" {
				controllerLease = candidate
				break
			}
		}
		rt.controllerLeases.mu.Unlock()
		if controllerLease != nil {
			controllerEvents, _ = readControllerInboxRecords(controllerLease.inbox.path)
			if controllerEventKinds(controllerEvents)["mount_result"] && controllerHasSessionDependencyEvent(controllerEvents, "JEA", remoteID) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	kinds := controllerEventKinds(controllerEvents)
	for _, want := range []string{"controller_started", "mount_result", "dependency_event", "session_mount_result"} {
		if !kinds[want] {
			state := map[string]any{"state": "missing"}
			stderr := ""
			if controllerLease != nil {
				state = controllerLease.state()
				body, _ := os.ReadFile(filepath.Join(controllerLease.RuntimeDir, "stderr.log"))
				stderr = string(body)
			}
			t.Fatalf("missing Controller event %q: events=%+v state=%+v stderr=%q inventory=%+v", want, controllerEvents, state, stderr, inventory)
		}
	}
	foundSessionMeta := false
	for _, record := range controllerEvents {
		event, _ := record.Event.(map[string]any)
		if event["kind"] != "session_mount_result" {
			continue
		}
		result, _ := event["result"].(map[string]any)
		structured, _ := result["structured_content"].(map[string]any)
		if structured["remote_session_id"] == remoteID {
			foundSessionMeta = true
		}
	}
	if !foundSessionMeta {
		t.Fatalf("Controller mounted MCP call did not receive validated Remote Session metadata: %+v", controllerEvents)
	}
	foundSessionInboxMeta := false
	for _, record := range controllerEvents {
		event, _ := record.Event.(map[string]any)
		if event["kind"] != "dependency_event" {
			continue
		}
		source, _ := event["source"].(map[string]any)
		if source["plugin"] != "JEA" || source["remote_session_id"] != remoteID {
			continue
		}
		payload, _ := event["event"].(map[string]any)
		structured, _ := payload["structured_content"].(map[string]any)
		if structured["remote_session_id"] == remoteID {
			foundSessionInboxMeta = true
		}
	}
	if !foundSessionInboxMeta {
		t.Fatalf("session-scoped dependency inbox did not receive Remote Session metadata: %+v", controllerEvents)
	}

	signalResult := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "signal", "remote_session_id": remoteID, "purpose": "approve coordinator checkpoint",
		"plugin": "Coordinator", "signal": "owner.approve", "data": map[string]any{"checkpoint": "review"},
	})
	if !statusOK(signalResult) {
		t.Fatalf("plugin signal=%+v", signalResult)
	}
	signalDeadline := time.Now().Add(2 * time.Second)
	foundOwnerSignal := false
	for time.Now().Before(signalDeadline) {
		controllerEvents, _ = readControllerInboxRecords(controllerLease.inbox.path)
		for _, record := range controllerEvents {
			event, _ := record.Event.(map[string]any)
			if event["kind"] != "owner_signal_received" {
				continue
			}
			if event["signal"] == "owner.approve" && event["remote_session_id"] == remoteID {
				data, _ := event["data"].(map[string]any)
				foundOwnerSignal = data["checkpoint"] == "review"
			}
		}
		if foundOwnerSignal {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !foundOwnerSignal {
		t.Fatalf("Controller did not receive validated owner signal: %+v", controllerEvents)
	}

	body, err := os.ReadFile(contributionLog)
	if err != nil {
		t.Fatalf("target MCP did not receive contribution manifest: %v", err)
	}
	var manifest resolvedPluginContributionFile
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Target != "Comet" || manifest.Revision == "" || len(manifest.Contributions) != 1 || manifest.Contributions[0].Content != guidance {
		t.Fatalf("contribution manifest=%+v", manifest)
	}
	if len(manifest.Contributions[0].Content) > 1024 || !strings.HasPrefix(manifest.Contributions[0].Revision, "sha256:") {
		t.Fatalf("contribution was not bounded/revisioned: %+v", manifest.Contributions[0])
	}

	inbox := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "inbox", "remote_session_id": remoteID, "purpose": "read controller events", "limit": 50, "wait_ms": 0,
	})
	if !statusOK(inbox) {
		t.Fatalf("plugin inbox=%+v", inbox)
	}
	items := asMapSlice(inbox["data"].(map[string]any)["items"])
	foundController := false
	for _, item := range items {
		if item["plugin"] == "Coordinator" && item["status"] == "succeeded" {
			foundController = true
		}
	}
	if !foundController {
		t.Fatalf("aggregate inbox did not include Controller: %+v", items)
	}

	updatedGuidance := guidance + "Reject unsupported claims.\n"
	if err := os.WriteFile(guidancePath, []byte(updatedGuidance), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "verify contribution revision pinning", "name": "comet-any", "arguments": map[string]any{},
	})
	if statusOK(changed) || errorCode(changed) != "skill_revision_changed" {
		t.Fatalf("changed contribution must invalidate Skill discovery revision: %+v", changed)
	}
}

func TestEnforceControllerMountGuards(t *testing.T) {
	mount := config.MCPPluginMount{
		Plugin: "JEA", Tool: "agent_spawn", Automatic: true,
		Guards: map[string]config.MCPPluginStringGuard{
			"path":    {Prefix: "/agents/comet-"},
			"sandbox": {Equals: "read-only"},
			"type":    {OneOf: []string{"explore", "implement", "review"}},
		},
	}
	if err := enforceControllerMountGuards(mount, map[string]any{
		"path": "/agents/comet-demo", "sandbox": "read-only", "type": "review",
	}); err != nil {
		t.Fatalf("valid guarded call rejected: %v", err)
	}
	for name, arguments := range map[string]map[string]any{
		"prefix":  {"path": "/agents/other", "sandbox": "read-only", "type": "review"},
		"equals":  {"path": "/agents/comet-demo", "sandbox": "workspace-write", "type": "review"},
		"one_of":  {"path": "/agents/comet-demo", "sandbox": "read-only", "type": "unknown"},
		"missing": {"path": "/agents/comet-demo", "type": "review"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := enforceControllerMountGuards(mount, arguments); err == nil {
				t.Fatalf("guard violation accepted: %+v", arguments)
			}
		})
	}
}

func controllerEventKinds(records []controllerInboxRecord) map[string]bool {
	out := map[string]bool{}
	for _, record := range records {
		if event, ok := record.Event.(map[string]any); ok {
			if kind, _ := event["kind"].(string); kind != "" {
				out[kind] = true
			}
		}
	}
	return out
}

func controllerHasSessionDependencyEvent(records []controllerInboxRecord, pluginName, remoteSessionID string) bool {
	for _, record := range records {
		event, _ := record.Event.(map[string]any)
		if event["kind"] != "dependency_event" {
			continue
		}
		source, _ := event["source"].(map[string]any)
		if source["plugin"] == pluginName && source["remote_session_id"] == remoteSessionID {
			return true
		}
	}
	return false
}

func writeControllerSidecar(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "controller.py")
	code := `#!/usr/bin/env python3
import json, sys

def send(value):
    sys.stdout.write(json.dumps(value, separators=(",", ":")) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    message = json.loads(line)
    kind = message.get("type")
    if kind == "init":
        send({"type":"ready"})
        send({"type":"emit","event":{"kind":"controller_started"}})
        send({"type":"call","id":"echo-1","mount":"echo","arguments":{"value":"controller"}})
    elif kind == "result" and message.get("id") == "echo-1":
        send({"type":"emit","event":{"kind":"mount_result","result":message.get("result")}})
    elif kind == "result" and message.get("id") == "echo-session":
        send({"type":"emit","event":{"kind":"session_mount_result","result":message.get("result")}})
    elif kind == "event" and message.get("source", {}).get("kind") == "session.opened":
        session_id = message.get("event", {}).get("remote_session_id")
        send({"type":"call","id":"echo-session","mount":"echo","remote_session_id":session_id,"arguments":{"value":"session"}})
    elif kind == "event" and message.get("source", {}).get("kind") == "owner.signal":
        event = message.get("event", {})
        send({"type":"emit","event":{"kind":"owner_signal_received","signal":event.get("signal"),"data":event.get("data",{}),"remote_session_id":message.get("source",{}).get("remote_session_id")}})
    elif kind == "event" and message.get("source", {}).get("kind") == "inbox":
        send({"type":"emit","event":{"kind":"dependency_event","source":message.get("source"),"event":message.get("event")}})
`
	if err := os.WriteFile(path, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeControllerTargetMCP(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "controller-target.py")
	code := `#!/usr/bin/env python3
import json, os, sys

if os.environ.get("MCPX_PLUGIN_CATALOG") != "1":
    manifest = os.environ.get("MCPX_PLUGIN_CONTRIBUTIONS_FILE", "")
    log = os.environ.get("CONTRIBUTION_LOG", "")
    if manifest and log:
        with open(manifest, "r", encoding="utf-8") as src, open(log, "w", encoding="utf-8") as dst:
            dst.write(src.read())

def send(value):
    sys.stdout.write(json.dumps(value, separators=(",", ":")) + "\n")
    sys.stdout.flush()

tools = [
  {"name":"echo","description":"echo","inputSchema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}},
  {"name":"inbox","description":"inbox","inputSchema":{"type":"object","properties":{"limit":{"type":"integer"},"wait_ms":{"type":"integer"},"cursor":{"type":"string"}},"required":["limit","wait_ms"],"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False,"idempotentHint":True,"openWorldHint":False}}
]
for line in sys.stdin:
    request = json.loads(line)
    rid = request.get("id")
    if rid is None: continue
    method = request.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":rid,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"controller-target","version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":rid,"result":{"tools":tools}})
    elif method == "tools/call":
        params = request.get("params", {})
        name = params.get("name")
        args = params.get("arguments", {})
        if name == "echo":
            meta = params.get("_meta", {})
            send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"echo:"+args.get("value","")}],"structuredContent":{"value":args.get("value",""),"remote_session_id":meta.get("mcpx/remote_session_id")},"isError":False}})
        elif name == "inbox":
            cursor = args.get("cursor", "")
            meta = params.get("_meta", {})
            remote_session_id = meta.get("mcpx/remote_session_id")
            items = [] if cursor else [{"kind":"dependency_ready","remote_session_id":remote_session_id}]
            send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"inbox"}],"structuredContent":{"items":items,"next_cursor":"1","remote_session_id":remote_session_id},"isError":False}})
        else:
            send({"jsonrpc":"2.0","id":rid,"error":{"code":-32602,"message":"unknown tool"}})
    else:
        send({"jsonrpc":"2.0","id":rid,"error":{"code":-32601,"message":"method not found"}})
`
	if err := os.WriteFile(path, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
