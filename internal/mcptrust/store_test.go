package mcptrust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePersistsAndReplacesRegistrationApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-trust.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if approved, err := store.IsApproved(workspace, "demo", "sha256:one"); err != nil || approved {
		t.Fatalf("unexpected initial approval=%v err=%v", approved, err)
	}
	if err := store.Approve(workspace, "demo", "sha256:one"); err != nil {
		t.Fatal(err)
	}
	if approved, err := store.IsApproved(workspace, "demo", "sha256:one"); err != nil || !approved {
		t.Fatalf("approval not persisted=%v err=%v", approved, err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if approved, err := reopened.IsApproved(workspace, "demo", "sha256:one"); err != nil || !approved {
		t.Fatalf("reopened approval=%v err=%v", approved, err)
	}
	if err := reopened.Approve(workspace, "demo", "sha256:two"); err != nil {
		t.Fatal(err)
	}
	if approved, _ := reopened.IsApproved(workspace, "demo", "sha256:one"); approved {
		t.Fatal("old fingerprint remained approved after registration changed")
	}
	if approved, err := reopened.IsApproved(workspace, "demo", "sha256:two"); err != nil || !approved {
		t.Fatalf("replacement approval=%v err=%v", approved, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("trust store permissions=%o", info.Mode().Perm())
	}
}

func TestStoreCanonicalizesWorkspaceSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "mcp-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(alias, "demo", "sha256:one"); err != nil {
		t.Fatal(err)
	}
	if approved, err := store.IsApproved(workspace, "demo", "sha256:one"); err != nil || !approved {
		t.Fatalf("physical Workspace did not match approved alias: approved=%v err=%v", approved, err)
	}
}

func TestOpenRejectsMalformedTrustStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-trust.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("malformed trust store should fail closed")
	}
}
