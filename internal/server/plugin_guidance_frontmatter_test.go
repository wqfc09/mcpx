package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPluginGuidanceAssetStripsPackageFrontMatterButHashesWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waiting.md")
	body := `---
id: demo.waiting
summary: Wait guidance
scope: context
---
Wait patiently.
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	content, revision, err := readPluginGuidanceAsset(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if content != "Wait patiently." || revision == "" {
		t.Fatalf("content=%q revision=%q", content, revision)
	}
}
