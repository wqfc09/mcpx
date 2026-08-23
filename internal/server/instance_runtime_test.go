package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
)

func TestRuntimeCloseStopsServeAndRemovesInstanceRendezvous(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "mcpx-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	runtimeDir, err := os.MkdirTemp("/tmp", "mcpx-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_RUNTIME_DIR", runtimeDir)
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
	rt, err := New(Options{AddrOverride: "127.0.0.1:0", InstanceID: "mcpx_runtime_close_test"})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- rt.Start() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		state, resolveErr := runtimeinstance.ResolveRunning()
		if resolveErr == nil {
			if state.InstanceID != rt.instanceID || state.PID != os.Getpid() {
				t.Fatalf("published Instance=%+v", state)
			}
			break
		}
		if !errors.Is(resolveErr, runtimeinstance.ErrNotFound) {
			t.Fatalf("resolve Instance: %v", resolveErr)
		}
		select {
		case <-ctx.Done():
			t.Fatal("Runtime did not publish Instance rendezvous")
		case <-time.After(20 * time.Millisecond):
		}
	}

	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime.Close did not stop Serve")
	}
	if _, err := runtimeinstance.Read(); !errors.Is(err, runtimeinstance.ErrNotFound) {
		t.Fatalf("Instance rendezvous remained after Close: %v", err)
	}
}
