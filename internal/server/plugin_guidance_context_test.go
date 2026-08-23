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
)

func installContextGuidancePlugin(t *testing.T, rt *Runtime, workspaceName, pluginName, guidanceID, body string) string {
	t.Helper()
	ws, ok := rt.reg.Get(workspaceName)
	if !ok {
		t.Fatalf("workspace %q missing", workspaceName)
	}
	guidancePath := filepath.Join(ws.Path, strings.ReplaceAll(guidanceID, ".", "-")+".md")
	if err := os.WriteFile(guidancePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	file := config.MCPFile{MCPServers: map[string]config.MCPServer{
		pluginName: {
			Command: writeQuietGuidanceController(t, ws.Path), IsPlugin: true, Trust: true, Enabled: &enabled,
			Plugin: &config.MCPPlugin{
				Runtime: config.PluginRuntimeNative, Scope: config.PluginScopeWorkspace,
				Guidance: []config.MCPPluginGuidance{{ID: guidanceID, Scope: config.PluginGuidanceScopeContext, Path: guidancePath}},
			},
		},
	}}
	if err := config.WriteMCPFile(config.ProjectMCPPath(ws.Path), file); err != nil {
		t.Fatal(err)
	}
	return guidancePath
}

func responseData(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	data, _ := response["data"].(map[string]any)
	if data == nil {
		t.Fatalf("response data missing: %+v", response)
	}
	return data
}

func TestPluginToolContextGuidanceExplicitBindingPinsRevision(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	guidancePath := installContextGuidancePlugin(t, rt, "demo", "Guide", "demo.build", "BUILD ABC\n")

	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "client_request_id": "parent-a",
	})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID, _ := opened["remote_session_id"].(string)
	attachmentBundle := attachmentGuidanceFromEnvelope(t, opened)
	if bindings := asMapSlice(attachmentBundle["bindings"]); len(bindings) != 0 {
		t.Fatalf("context Guidance must not auto-bind to parent Attachment: %+v", attachmentBundle)
	}

	listed := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "guidance_list", "remote_session_id": remoteID,
	})
	if !statusOK(listed) {
		t.Fatalf("guidance_list=%+v", listed)
	}
	listedData := responseData(t, listed)
	metadata := asMapSlice(listedData["guidance"])
	if len(metadata) != 1 || metadata[0]["guidance_id"] != "demo.build" || metadata[0]["provider_plugin"] != "Guide" || metadata[0]["scope"] != config.PluginGuidanceScopeContext {
		t.Fatalf("Guidance metadata=%+v", listedData)
	}
	listedJSON, _ := json.Marshal(listed)
	if strings.Contains(string(listedJSON), "BUILD ABC") {
		t.Fatalf("guidance_list leaked body: %s", listedJSON)
	}

	bind := func(consumerID, key string) map[string]any {
		response := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
			"action": "guidance_bind", "remote_session_id": remoteID,
			"purpose": "create explicit model context", "guidance_id": "demo.build",
			"consumer_id": consumerID, "idempotency_key": key,
		})
		if !statusOK(response) {
			t.Fatalf("guidance_bind consumer=%q key=%q: %+v", consumerID, key, response)
		}
		return responseData(t, response)
	}

	first := bind("/agents/builder", "bind-1")
	if first["delivered"] != true || first["content"] != "BUILD ABC\n" || first["consumer_id"] != "/agents/builder" {
		t.Fatalf("first context bind=%+v", first)
	}
	revisionABC, _ := first["revision"].(string)
	if revisionABC == "" || first["current_revision"] != revisionABC {
		t.Fatalf("first revisions=%+v", first)
	}

	if err := os.WriteFile(guidancePath, []byte("BUILD DEF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := bind("/agents/builder", "bind-2")
	if later["delivered"] != false {
		t.Fatalf("existing consumer unexpectedly re-delivered body: %+v", later)
	}
	if _, exists := later["content"]; exists {
		t.Fatalf("existing consumer leaked body on later bind: %+v", later)
	}
	if later["revision"] != revisionABC || later["current_revision"] == revisionABC {
		t.Fatalf("existing consumer was not pinned while current revision advanced: %+v", later)
	}
	revisionDEF, _ := later["current_revision"].(string)

	retry := bind("/agents/builder", "bind-1")
	if retry["delivered"] != true || retry["content"] != "BUILD ABC\n" || retry["revision"] != revisionABC || retry["current_revision"] != revisionDEF {
		t.Fatalf("exact retry did not replay pinned ABC against current DEF: %+v", retry)
	}

	fresh := bind("/agents/builder-2", "bind-3")
	if fresh["delivered"] != true || fresh["content"] != "BUILD DEF\n" || fresh["revision"] != revisionDEF {
		t.Fatalf("new consumer did not receive current DEF: %+v", fresh)
	}
}

func writeGuidanceBindingController(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "guidance-binding-controller.py")
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
        send({"type":"guidance_bind","id":"bind-unattached","remote_session_id":"rs_missing","guidance_id":"demo.controller","consumer_id":"/agents/unattached","idempotency_key":"unattached-1","purpose":"prove attached-session guard"})
    elif kind == "result" and message.get("id") == "bind-unattached":
        send({"type":"emit","event":{"kind":"guidance_bind_unattached_result","ok":message.get("ok"),"error":message.get("error")}})
    elif kind == "event" and message.get("source", {}).get("kind") == "session.opened":
        session_id = message.get("event", {}).get("remote_session_id")
        send({"type":"guidance_bind","id":"bind-attached","remote_session_id":session_id,"guidance_id":"demo.controller","consumer_id":"/agents/controller-builder","idempotency_key":"controller-bind-1","purpose":"create controller builder context"})
    elif kind == "result" and message.get("id") == "bind-attached":
        send({"type":"emit","event":{"kind":"guidance_bind_attached_result","ok":message.get("ok"),"error":message.get("error"),"result":message.get("result")}})
`
	if err := os.WriteFile(path, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestControllerContextGuidanceRequiresAttachedSessionAndBindsOwnContribution(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	ws, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("workspace demo missing")
	}
	guidancePath := filepath.Join(ws.Path, "controller-guidance.md")
	if err := os.WriteFile(guidancePath, []byte("CONTROLLER BUILD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	file := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"Guide": {
			Command: writeGuidanceBindingController(t, ws.Path), IsPlugin: true, Trust: true, Enabled: &enabled,
			Plugin: &config.MCPPlugin{
				Runtime: config.PluginRuntimeNative, Scope: config.PluginScopeWorkspace,
				Guidance: []config.MCPPluginGuidance{{ID: "demo.controller", Scope: config.PluginGuidanceScopeContext, Path: guidancePath}},
			},
		},
	}}
	if err := config.WriteMCPFile(config.ProjectMCPPath(ws.Path), file); err != nil {
		t.Fatal(err)
	}

	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "client_request_id": "controller-parent",
	})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID, _ := opened["remote_session_id"].(string)

	deadline := time.Now().Add(5 * time.Second)
	var events []controllerInboxRecord
	var lease *controllerRuntimeLease
	for time.Now().Before(deadline) {
		rt.controllerLeases.mu.Lock()
		lease = nil
		for _, candidate := range rt.controllerLeases.leases {
			if candidate.Plugin == "Guide" {
				lease = candidate
				break
			}
		}
		rt.controllerLeases.mu.Unlock()
		if lease != nil {
			events, _ = readControllerInboxRecords(lease.inbox.path)
			kinds := controllerEventKinds(events)
			if kinds["guidance_bind_unattached_result"] && kinds["guidance_bind_attached_result"] {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lease == nil {
		t.Fatal("Guide Controller lease did not start")
	}

	var sawUnattached, sawAttached bool
	for _, record := range events {
		event, _ := record.Event.(map[string]any)
		switch event["kind"] {
		case "guidance_bind_unattached_result":
			sawUnattached = true
			errorText, _ := event["error"].(string)
			if event["ok"] != false || !strings.Contains(strings.ToLower(errorText), "not attached") {
				t.Fatalf("unattached Guidance bind was not rejected: %+v", event)
			}
		case "guidance_bind_attached_result":
			sawAttached = true
			if event["ok"] != true {
				t.Fatalf("attached Guidance bind failed: %+v", event)
			}
			result, _ := event["result"].(map[string]any)
			if result["provider_plugin"] != "Guide" || result["consumer_id"] != "/agents/controller-builder" || result["content"] != "CONTROLLER BUILD\n" || result["delivered"] != true {
				t.Fatalf("attached Guidance bind result=%+v remote=%s", result, remoteID)
			}
		}
	}
	if !sawUnattached || !sawAttached {
		t.Fatalf("missing Controller Guidance results: events=%+v state=%+v", events, lease.state())
	}
}

func TestContextGuidanceProviderConstraintPreventsCrossPluginBinding(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	installContextGuidancePlugin(t, rt, "demo", "Guide", "demo.build", "BUILD\n")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "client_request_id": "parent-provider-guard",
	})
	if !statusOK(opened) {
		t.Fatalf("session open=%+v", opened)
	}
	remoteID, _ := opened["remote_session_id"].(string)
	ws, _ := rt.reg.Get("demo")
	_, err := rt.bindContextGuidance(context.Background(), remoteID, ws.Path, "demo.build", "/agents/builder", "bind-guard", "OtherPlugin")
	if err == nil || !strings.Contains(err.Error(), "cannot bind Guidance") {
		t.Fatalf("provider constraint err=%v", err)
	}
}
