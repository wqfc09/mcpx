package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/lockfile"
)

func TestRegistryReadsDurableConfigLive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	alpha := filepath.Join(home, "alpha")
	beta := filepath.Join(home, "beta")
	for _, path := range []string{alpha, beta} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	global := filepath.Join(home, "config.yaml")
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "alpha", Path: alpha, Description: "first"}}
	if err := config.WriteGlobal(global, cfg); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}
	list, err := registry.ListChecked()
	if err != nil || len(list) != 1 || list[0].Status != StatusOK {
		t.Fatalf("initial list=%+v err=%v", list, err)
	}

	cfg.Workspaces = append(cfg.Workspaces, config.WorkspaceEntry{Name: "beta", Path: beta})
	if err := config.WriteGlobal(global, cfg); err != nil {
		t.Fatal(err)
	}
	list, err = registry.ListChecked()
	if err != nil || len(list) != 2 || list[1].Name != "beta" {
		t.Fatalf("live list=%+v err=%v", list, err)
	}
	resolved, err := registry.Resolve("beta")
	if err != nil || resolved.Path != canonicalWorkspacePath(beta) {
		t.Fatalf("live resolve=%+v err=%v", resolved, err)
	}
}

func TestRegistryMutationWaitsForSharedLockAndReloads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	global := filepath.Join(home, "config.yaml")
	if err := config.WriteGlobal(global, config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}
	alpha := filepath.Join(home, "alpha")
	beta := filepath.Join(home, "beta")
	for _, path := range []string{alpha, beta} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	guard, err := lockfile.Acquire(registryMutationLockPath(global), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, registerErr := registry.Register("beta", beta)
		done <- registerErr
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("Registry mutation bypassed shared lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cfg, err := config.LoadGlobal(global)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = append(cfg.Workspaces, config.WorkspaceEntry{Name: "alpha", Path: alpha})
	if err := config.WriteGlobal(global, cfg); err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Registry mutation did not resume after shared lock release")
	}
	list, err := registry.ListChecked()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("concurrent Registry updates were not preserved: %+v", list)
	}
}

func TestRegistryLifecycleMutatesOnlyDurableRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	global := filepath.Join(home, "config.yaml")
	if err := config.WriteGlobal(global, config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(home, "first")
	second := filepath.Join(home, "second")
	third := filepath.Join(home, "third")
	for _, path := range []string{first, second, third} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	registered, err := registry.Register("", first)
	if err != nil || registered.Name != "first" || registered.Status != StatusOK {
		t.Fatalf("default register=%+v err=%v", registered, err)
	}
	custom, err := registry.Register("custom", second)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := registry.Register("custom", third)
	if err != nil || updated.Path != canonicalWorkspacePath(third) || updated.ID != custom.ID {
		t.Fatalf("upsert=%+v original=%+v err=%v", updated, custom, err)
	}
	renamed, err := registry.Rename("custom", "renamed")
	if err != nil || renamed.Name != "renamed" || renamed.Path != canonicalWorkspacePath(third) || renamed.ID != custom.ID {
		t.Fatalf("rename=%+v err=%v", renamed, err)
	}
	removed, err := registry.Unregister("first")
	if err != nil || removed.Path != canonicalWorkspacePath(first) {
		t.Fatalf("unregister=%+v err=%v", removed, err)
	}
	if info, statErr := os.Stat(first); statErr != nil || !info.IsDir() {
		t.Fatalf("unregister touched Workspace path: info=%v err=%v", info, statErr)
	}
	if _, ok := registry.Get("first"); ok {
		t.Fatal("unregistered entry still present")
	}
	resolvedByID, err := registry.ResolveID(custom.ID)
	if err != nil || resolvedByID.Name != "renamed" || resolvedByID.Path != canonicalWorkspacePath(third) {
		t.Fatalf("resolve stable id=%+v err=%v", resolvedByID, err)
	}
}

func TestRegistryEmptyNameReusesExistingPathRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	global := filepath.Join(home, "config.yaml")
	if err := config.WriteGlobal(global, config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "workspace-dir")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := registry.Register("custom-name", path)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := registry.Register("", path)
	if err != nil {
		t.Fatal(err)
	}
	if reused.Name != "custom-name" || reused.ID != first.ID || reused.Path != first.Path {
		t.Fatalf("reused=%+v first=%+v", reused, first)
	}
	listed, err := registry.ListChecked()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "custom-name" {
		t.Fatalf("registrations=%+v", listed)
	}
}

func TestRegistryRejectsDuplicatePathOwnership(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	global := filepath.Join(home, "config.yaml")
	if err := config.WriteGlobal(global, config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "shared")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register("one", path); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register("two", path); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate path err=%v", err)
	}
}

func TestRegistryPruneDryRunAndApply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	global := filepath.Join(home, "config.yaml")
	live := filepath.Join(home, "live")
	missing := filepath.Join(home, "missing")
	invalid := filepath.Join(home, "not-a-directory")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{
		{Name: "live", Path: live},
		{Name: "missing", Path: missing},
		{Name: "invalid", Path: invalid},
	}
	if err := config.WriteGlobal(global, cfg); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := registry.Prune(false)
	if err != nil || len(stale) != 2 || stale[0].Status != StatusMissing || stale[1].Status != StatusInvalid {
		t.Fatalf("dry-run stale=%+v err=%v", stale, err)
	}
	list := registry.List()
	if len(list) != 3 {
		t.Fatalf("dry-run mutated registry: %+v", list)
	}
	stale, err = registry.Prune(true)
	if err != nil || len(stale) != 2 {
		t.Fatalf("apply stale=%+v err=%v", stale, err)
	}
	list, err = registry.ListChecked()
	if err != nil || len(list) != 1 || list[0].Name != "live" {
		t.Fatalf("post-prune list=%+v err=%v", list, err)
	}
	if content, err := os.ReadFile(invalid); err != nil || string(content) != "file" {
		t.Fatalf("prune touched Workspace filesystem: content=%q err=%v", content, err)
	}
}
