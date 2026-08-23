package pluginstate

import (
	"os"
	"testing"
)

func TestStatusWriteReadRemove(t *testing.T) {
	runtimeDir := t.TempDir()
	want := Status{
		Plugin: "Demo", Runtime: "mcp", Scope: "workspace", WorkspaceID: "ws1", WorkspaceName: "demo",
		PID: 1234, Executable: "/bin/demo", Argv0: "demo", Args: []string{"serve", "--mode=plugin"}, StartedAt: "2026-08-16T00:00:00Z", RuntimeRevision: "sha256:demo",
	}
	if err := Write(runtimeDir, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(runtimeDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lease mode=%o want=600", got)
	}
	got, err := Read(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version || got.Plugin != want.Plugin || got.Runtime != want.Runtime || got.Scope != want.Scope || got.WorkspaceID != want.WorkspaceID || got.PID != want.PID || got.Executable != want.Executable || got.Argv0 != want.Argv0 || len(got.Args) != len(want.Args) || got.Args[0] != want.Args[0] || got.Args[1] != want.Args[1] || got.RuntimeRevision != want.RuntimeRevision {
		t.Fatalf("lease status=%+v want=%+v", got, want)
	}
	if err := Remove(runtimeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("lease status was not removed: %v", err)
	}
	if err := Remove(runtimeDir); err != nil {
		t.Fatalf("remove should be idempotent: %v", err)
	}
}

func TestStatusReadRejectsInvalidIdentity(t *testing.T) {
	runtimeDir := t.TempDir()
	if err := os.WriteFile(Path(runtimeDir), []byte(`{"version":1,"plugin":"Demo","pid":12,"executable":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(runtimeDir); err == nil {
		t.Fatal("invalid lease identity was accepted")
	}
}
