package server

import (
	"mcpx/internal/mcpresult"

	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
)

func newWorkspaceRuntime(t *testing.T, names ...string) *Runtime {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	entries := make([]config.WorkspaceEntry, 0, len(names))
	for _, name := range names {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, config.WorkspaceEntry{Name: name, Path: path})
	}
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Workspaces = entries
	cfg.Logging.Enabled = false
	cfg.Logging.Dir = filepath.Join(home, "logs")
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestResolveExplicitWorkspaceByName(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws, remoteID, err := rt.resolveExplicitWorkspace(context.Background(), principal, envelope.Request{
		Workspace: "demo", Payload: map[string]any{},
	})
	if err != nil || remoteID != "" || ws.Name != "demo" {
		t.Fatalf("workspace=%+v remote=%q err=%v", ws, remoteID, err)
	}
}

func TestResolveRemoteSessionWorkspaceWithoutTransportBinding(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceID: registered.ID, WorkspaceName: "demo", WorkspacePath: registered.Path, Label: "explicit session",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Request{RemoteSessionID: created.Session.ID, Payload: map[string]any{}}
	for i := 0; i < 2; i++ {
		ws, remoteID, err := rt.resolveExplicitWorkspace(context.Background(), principal, request)
		if err != nil || remoteID != created.Session.ID || ws.Path != registered.Path {
			t.Fatalf("iteration=%d workspace=%+v remote=%q err=%v", i, ws, remoteID, err)
		}
	}
}

func TestResolveRemoteSessionTracksRegistryRenameAndPathMigration(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceID: registered.ID, WorkspaceName: registered.Name, WorkspacePath: registered.Path, Label: "stable identity",
	})
	if err != nil {
		t.Fatal(err)
	}
	migrated := filepath.Join(filepath.Dir(registered.Path), "migrated")
	if err := os.MkdirAll(migrated, 0o755); err != nil {
		t.Fatal(err)
	}
	updated, err := rt.reg.Register("demo", migrated)
	if err != nil || updated.ID != registered.ID {
		t.Fatalf("path migration=%+v err=%v", updated, err)
	}
	renamed, err := rt.reg.Rename("demo", "renamed")
	if err != nil || renamed.ID != registered.ID {
		t.Fatalf("rename=%+v err=%v", renamed, err)
	}
	ws, _, err := rt.resolveExplicitWorkspace(context.Background(), principal, envelope.Request{RemoteSessionID: created.Session.ID, Payload: map[string]any{}})
	if err != nil || ws.ID != registered.ID || ws.Name != "renamed" || ws.Path != renamed.Path {
		t.Fatalf("resolved current workspace=%+v err=%v", ws, err)
	}
}

func TestResolveRemoteSessionFailsAfterWorkspaceUnregister(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceID: registered.ID, WorkspaceName: registered.Name, WorkspacePath: registered.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.reg.Unregister("demo"); err != nil {
		t.Fatal(err)
	}
	_, _, err = rt.resolveExplicitWorkspace(context.Background(), principal, envelope.Request{RemoteSessionID: created.Session.ID, Payload: map[string]any{}})
	if !errors.Is(err, errWorkspaceUnregistered) {
		t.Fatalf("expected workspace unregistered, got %v", err)
	}
}

func TestWorkspaceInfoDoesNotAutoSelectSingleWorkspace(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	req := mcpresult.Request(map[string]any{"intent": "inspect the project workspace", "action": "project"})

	out, err := rt.toolRuntimeInspect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeToolResult(t, out)
	errObj, _ := env["error"].(map[string]any)
	if env["status"] != "failed" || errObj["code"] != "REMOTE_SESSION_REQUIRED" {
		// code may live on wire error or nested data depending on wrap path
		if data, _ := env["data"].(map[string]any); data != nil {
			if nested, _ := data["error"].(map[string]any); nested != nil {
				errObj = nested
			}
		}
		if env["status"] != "failed" || errObj["code"] != "REMOTE_SESSION_REQUIRED" {
			t.Fatalf("expected explicit workspace error, got %+v", env)
		}
	}
}

func TestRegisteredToolsExcludeCompatibilityInterfaces(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	rt.registerTools(protocol)
	tools := rt.listedToolMap()
	for _, removed := range []string{"workspace_select", "code_read", "file_patch"} {
		if _, exists := tools[removed]; exists {
			t.Fatalf("removed compatibility tool %q is still registered", removed)
		}
	}
	for _, required := range []string{"session", "read", "edit", "observe"} {
		if _, exists := tools[required]; !exists {
			t.Fatalf("required tool %q is missing", required)
		}
	}
}
