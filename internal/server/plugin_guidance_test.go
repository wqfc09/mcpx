package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcpx/internal/config"
)

func writeQuietGuidanceController(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "guidance-controller.py")
	code := `#!/usr/bin/env python3
import json, sys
for line in sys.stdin:
    message = json.loads(line)
    if message.get("type") == "init":
        sys.stdout.write('{"type":"ready"}\n')
        sys.stdout.flush()
`
	if err := os.WriteFile(path, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func installAttachmentGuidancePlugin(t *testing.T, rt *Runtime, workspaceName, body string) (string, string) {
	t.Helper()
	ws, ok := rt.reg.Get(workspaceName)
	if !ok {
		t.Fatalf("workspace %q missing", workspaceName)
	}
	guidancePath := filepath.Join(ws.Path, "coordination.md")
	if err := os.WriteFile(guidancePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	file := config.MCPFile{MCPServers: map[string]config.MCPServer{
		"Guide": {
			Command: writeQuietGuidanceController(t, ws.Path), IsPlugin: true, Trust: true, Enabled: &enabled,
			Plugin: &config.MCPPlugin{
				Runtime: config.PluginRuntimeNative, Scope: config.PluginScopeWorkspace,
				Guidance: []config.MCPPluginGuidance{{ID: "demo.coordination", Scope: config.PluginGuidanceScopeAttachment, Path: guidancePath}},
			},
		},
	}}
	if err := config.WriteMCPFile(config.ProjectMCPPath(ws.Path), file); err != nil {
		t.Fatal(err)
	}
	return ws.Path, guidancePath
}

func attachmentGuidanceFromEnvelope(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	data, _ := response["data"].(map[string]any)
	bundle, _ := data["attachment_guidance"].(map[string]any)
	if bundle == nil {
		t.Fatalf("attachment_guidance missing: %+v", response)
	}
	return bundle
}

func TestSessionOpenBindsAttachmentGuidanceOncePerAttachmentAndPinsRevision(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	_, guidancePath := installAttachmentGuidancePlugin(t, rt, "demo", "coordination ABC\n")

	first := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "client_request_id": "model-context-a",
	})
	remoteID, _ := first["remote_session_id"].(string)
	firstData, _ := first["data"].(map[string]any)
	attachmentA, _ := firstData["attachment_id"].(string)
	bundleA := attachmentGuidanceFromEnvelope(t, first)
	bindingsA, _ := bundleA["bindings"].([]any)
	if len(bindingsA) != 1 {
		t.Fatalf("attachment A bindings=%+v", bundleA)
	}
	bindingA, _ := bindingsA[0].(map[string]any)
	if bindingA["consumer_id"] != attachmentA || bindingA["content"] != "coordination ABC\n" || bindingA["delivered"] != true {
		t.Fatalf("attachment A delivery=%+v", bindingA)
	}
	revisionABC, _ := bindingA["revision"].(string)
	currentABC, _ := bundleA["current_revision"].(string)
	if revisionABC == "" || currentABC == "" {
		t.Fatalf("attachment A revisions missing: %+v", bundleA)
	}

	listed := callEnvelope(t, rt.toolPluginTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID,
	})
	encodedList, _ := json.Marshal(listed)
	if strings.Contains(string(encodedList), "coordination ABC") {
		t.Fatalf("ordinary plugin_tool call repeated Attachment Guidance body: %s", encodedList)
	}

	if err := os.WriteFile(guidancePath, []byte("coordination DEF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retry := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "client_request_id": "model-context-a",
	})
	retryData, _ := retry["data"].(map[string]any)
	if retryData["attachment_id"] != attachmentA {
		t.Fatalf("exact session retry changed attachment: %v != %q", retryData["attachment_id"], attachmentA)
	}
	retryBundle := attachmentGuidanceFromEnvelope(t, retry)
	retryBindings, _ := retryBundle["bindings"].([]any)
	retryBinding, _ := retryBindings[0].(map[string]any)
	if retryBinding["revision"] != revisionABC || retryBinding["content"] != "coordination ABC\n" || retryBinding["delivered"] != true {
		t.Fatalf("exact retry must replay pinned ABC: %+v", retryBinding)
	}
	if retryBundle["current_revision"] == currentABC {
		t.Fatalf("current Attachment Guidance revision did not reflect DEF: %+v", retryBundle)
	}

	second := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "remote_session_id": remoteID, "client_request_id": "model-context-b",
	})
	secondData, _ := second["data"].(map[string]any)
	attachmentB, _ := secondData["attachment_id"].(string)
	if attachmentB == "" || attachmentB == attachmentA {
		t.Fatalf("new model context attachment=%q first=%q", attachmentB, attachmentA)
	}
	bundleB := attachmentGuidanceFromEnvelope(t, second)
	bindingsB, _ := bundleB["bindings"].([]any)
	bindingB, _ := bindingsB[0].(map[string]any)
	if bindingB["consumer_id"] != attachmentB || bindingB["content"] != "coordination DEF\n" || bindingB["revision"] == revisionABC || bindingB["delivered"] != true {
		t.Fatalf("attachment B must bind current DEF: %+v", bindingB)
	}
}

func TestSessionOpenWithoutPluginGuidanceKeepsOptionalBundleEmpty(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "client_request_id": "plain-context",
	})
	bundle := attachmentGuidanceFromEnvelope(t, opened)
	bindings, ok := bundle["bindings"].([]any)
	if !ok || len(bindings) != 0 {
		t.Fatalf("Plugin without Guidance changed bootstrap: %+v", bundle)
	}
	if revision, _ := bundle["current_revision"].(string); !strings.HasPrefix(revision, "sha256:") {
		t.Fatalf("empty Guidance set revision=%q", revision)
	}
}

func TestReadPluginGuidanceAssetRejectsOversizeAndInvalidText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guidance.md")
	if err := os.WriteFile(path, make([]byte, config.PluginGuidanceHardMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPluginGuidanceAsset(path); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversize Guidance err=%v", err)
	}
	if err := os.WriteFile(path, []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPluginGuidanceAsset(path); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 Guidance err=%v", err)
	}
}
