package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/auth"
	"mcpx/internal/config"
)

func TestWorkspaceListDoesNotRequireRemoteSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)

	alpha := filepath.Join(home, "alpha")
	beta := filepath.Join(home, "beta")
	for _, path := range []string{alpha, beta} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "bearer"
	cfg.Auth.Token = "workspace-token"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{
		{Name: "alpha", Path: alpha, Description: "Alpha workspace"},
		{Name: "beta", Path: beta, Description: "Beta workspace"},
	}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}

	runtime, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	ctx := auth.ContextWithAuthorization(context.Background(), "Bearer workspace-token")
	response := callEnvelope(t, runtime.toolWorkspace, ctx, map[string]any{})
	if !statusOK(response) {
		t.Fatalf("workspace list failed: %+v", response)
	}
	items := workspaceItems(t, response)
	if len(items) != 2 {
		t.Fatalf("workspaces=%+v", items)
	}
	first := items[0]
	second := items[1]
	if first["name"] != "alpha" || first["path"] != alpha || first["description"] != "Alpha workspace" || first["status"] != "ok" {
		t.Fatalf("first workspace=%+v", first)
	}
	if second["name"] != "beta" || second["path"] != beta || second["description"] != "Beta workspace" || second["status"] != "ok" {
		t.Fatalf("second workspace=%+v", second)
	}
}

func TestWorkspaceRegistryReloadsDurableConfigWithoutRuntimeRestart(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "alpha")
	home, err := config.HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	beta := filepath.Join(home, "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	global, err := config.GlobalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadGlobal(global)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = append(cfg.Workspaces, config.WorkspaceEntry{Name: "beta", Path: beta})
	if err := config.WriteGlobal(global, cfg); err != nil {
		t.Fatal(err)
	}

	listed := callEnvelope(t, runtime.toolWorkspace, context.Background(), map[string]any{})
	if !statusOK(listed) {
		t.Fatalf("workspace list after external config write=%+v", listed)
	}
	items := workspaceItems(t, listed)
	if len(items) != 2 || items[1]["name"] != "beta" || items[1]["status"] != "ok" {
		t.Fatalf("Runtime did not reload durable registry: %+v", items)
	}

	opened := callEnvelope(t, runtime.toolSession, context.Background(), map[string]any{"workspace": "beta"})
	if !statusOK(opened) {
		t.Fatalf("newly registered Workspace could not open Session without restart: %+v", opened)
	}
	data := opened["data"].(map[string]any)
	workspaceData := data["workspace"].(map[string]any)
	if workspaceData["name"] != "beta" || workspaceData["path"] != beta {
		t.Fatalf("opened Workspace=%+v", workspaceData)
	}
}

func TestWorkspaceUnregisterBlocksNewSessionButExistingSessionKeepsStoredPath(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "alpha")
	opened := callEnvelope(t, runtime.toolSession, context.Background(), map[string]any{"workspace": "alpha"})
	if !statusOK(opened) {
		t.Fatalf("initial session=%+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)
	storedPath := opened["data"].(map[string]any)["workspace"].(map[string]any)["path"]

	if _, err := runtime.reg.Unregister("alpha"); err != nil {
		t.Fatal(err)
	}
	listed := callEnvelope(t, runtime.toolWorkspace, context.Background(), map[string]any{})
	if !statusOK(listed) || len(workspaceItems(t, listed)) != 0 {
		t.Fatalf("unregister not visible to Runtime list: %+v", listed)
	}

	newSession := callEnvelope(t, runtime.toolSession, context.Background(), map[string]any{"workspace": "alpha"})
	if statusOK(newSession) || envelopeErrorCode(newSession) != "WORKSPACE_NOT_FOUND" {
		t.Fatalf("new Session should be blocked after unregister: %+v", newSession)
	}

	resumed := callEnvelope(t, runtime.toolSession, context.Background(), map[string]any{"remote_session_id": remoteID})
	if !statusOK(resumed) {
		t.Fatalf("existing Session should survive unregister: %+v", resumed)
	}
	resumedPath := resumed["data"].(map[string]any)["workspace"].(map[string]any)["path"]
	if resumedPath != storedPath {
		t.Fatalf("existing Session path changed: got=%v want=%v", resumedPath, storedPath)
	}
}

func TestWorkspaceListMarksMissingPathAndNewSessionFailsUnavailable(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "beta")
	registered, ok := runtime.reg.Get("beta")
	if !ok {
		t.Fatal("beta Workspace missing")
	}
	if err := os.Remove(registered.Path); err != nil {
		t.Fatal(err)
	}

	listed := callEnvelope(t, runtime.toolWorkspace, context.Background(), map[string]any{})
	if !statusOK(listed) {
		t.Fatalf("workspace list=%+v", listed)
	}
	items := workspaceItems(t, listed)
	if len(items) != 1 || items[0]["status"] != "missing" {
		t.Fatalf("missing Workspace status=%+v", items)
	}

	opened := callEnvelope(t, runtime.toolSession, context.Background(), map[string]any{"workspace": "beta"})
	if statusOK(opened) || envelopeErrorCode(opened) != "WORKSPACE_UNAVAILABLE" {
		t.Fatalf("missing Workspace should reject new Session: %+v", opened)
	}
}

func workspaceItems(t *testing.T, response map[string]any) []map[string]any {
	t.Helper()
	data, _ := response["data"].(map[string]any)
	raw, _ := data["workspaces"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if mapped, ok := item.(map[string]any); ok {
			items = append(items, mapped)
		}
	}
	return items
}

func envelopeErrorCode(response map[string]any) string {
	if errObj, ok := response["error"].(map[string]any); ok {
		if code, _ := errObj["code"].(string); code != "" {
			return code
		}
	}
	if data, ok := response["data"].(map[string]any); ok {
		if errObj, ok := data["error"].(map[string]any); ok {
			code, _ := errObj["code"].(string)
			return code
		}
	}
	return ""
}
