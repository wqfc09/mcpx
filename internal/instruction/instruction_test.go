package instruction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAndReadGlobalThenProjectInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "system_prompt.md"), []byte("# Global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	documents := Discover(root)
	if len(documents) != 2 || documents[0].ID != "global" || documents[1].ID != "project" {
		t.Fatalf("documents: %+v", documents)
	}
	if documents[0].Name != "system_prompt.md" || documents[0].Priority != 10 || documents[1].Name != "AGENTS.md" || documents[1].Priority != 30 {
		t.Fatalf("instruction identities: %+v", documents)
	}
	document, content, err := Read(root, "project")
	if err != nil || document.Scope != "project" || content != "# Project\n" {
		t.Fatalf("read: document=%+v content=%q err=%v", document, content, err)
	}
}

func TestDiscoverAtNestedDirectoryChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "system_prompt.md"), []byte("# Global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "frontend", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(root, "AGENTS.md"):                    "# Project\n",
		filepath.Join(root, "frontend", "AGENTS.md"):        "# FE\n",
		filepath.Join(root, "frontend", "src", "AGENTS.md"): "# SRC\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	docs := DiscoverAt(root, "frontend/src/views/Home.vue")
	if len(docs) != 4 {
		t.Fatalf("want 4 docs, got %d %+v", len(docs), docs)
	}
	if docs[0].Scope != "global" || docs[1].Scope != "project" || docs[2].ID != "dir:frontend" || docs[3].ID != "dir:frontend/src" {
		t.Fatalf("unexpected chain: %+v", docs)
	}
	if docs[2].Priority != 40 || docs[3].Priority != 50 {
		t.Fatalf("directory priorities: %+v", docs)
	}
	backend := DiscoverAt(root, "backend/main.go")
	for _, doc := range backend {
		if strings.HasPrefix(doc.ID, "dir:frontend") {
			t.Fatalf("backend anchor must not load frontend rules: %+v", backend)
		}
	}
}

func TestDiscoverSkipsSymlinkAndOversizedDocuments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if documents := Discover(root); len(documents) != 0 {
		t.Fatalf("symlink must not be exposed: %+v", documents)
	}
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), make([]byte, MaxDocumentBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if documents := Discover(root); len(documents) != 0 {
		t.Fatalf("oversized document must not be exposed: %+v", documents)
	}
}
