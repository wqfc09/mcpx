package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPluginPackageV2NormalizesSplitFilesAndGuidanceMetadata(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("plugin.yaml", `manifest_version: 2
name: Demo
description: Demo package
runtime: ./runtime.yaml
tools: ./tools.yaml
guidance: ./guidance
ui:
  tui:
    title: Demo
    command: ./bin/monitor
    args: [./src/monitor.js]
`)
	mustWrite("runtime.yaml", `type: mcp
scope: workspace
command: ./bin/demo
args: [./src/cli.js, plugin]
`)
	mustWrite("tools.yaml", `tools: [echo, status]
inbox: inbox
`)
	mustWrite("guidance/waiting.md", `---
id: demo.waiting
summary: Wait guidance
scope: context
---
Wait patiently and use the Plugin Inbox.
`)

	got, err := LoadPluginManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestVersion != 2 || got.Name != "Demo" || got.Server.Plugin == nil {
		t.Fatalf("package normalization=%+v", got)
	}
	if got.Server.Command != filepath.Join(root, "bin", "demo") || got.Server.Args[0] != filepath.Join(root, "src", "cli.js") {
		t.Fatalf("runtime paths=%+v", got.Server)
	}
	plugin := got.Server.Plugin
	if len(plugin.Tools) != 2 || plugin.Tools[0] != "echo" || plugin.Inbox != "inbox" {
		t.Fatalf("tools=%+v", plugin)
	}
	if len(plugin.Guidance) != 1 {
		t.Fatalf("guidance=%+v", plugin.Guidance)
	}
	guidance := plugin.Guidance[0]
	if guidance.ID != "demo.waiting" || guidance.Summary != "Wait guidance" || guidance.Scope != PluginGuidanceScopeContext || !guidance.FrontMatter || guidance.Path != filepath.Join(root, "guidance", "waiting.md") {
		t.Fatalf("guidance metadata=%+v", guidance)
	}
	if plugin.TUI == nil || plugin.TUI.Command != filepath.Join(root, "bin", "monitor") || plugin.TUI.Args[0] != filepath.Join(root, "src", "monitor.js") {
		t.Fatalf("tui=%+v", plugin.TUI)
	}

	byFile, err := LoadPluginManifest(filepath.Join(root, "plugin.yaml"))
	if err != nil || byFile.Name != got.Name {
		t.Fatalf("file load=%+v err=%v", byFile, err)
	}
}

func TestLoadPluginPackageV2StrictlyRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plugin.yaml"), []byte(`manifest_version: 2
name: Demo
runtime: ./runtime.yaml
mystery: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.yaml"), []byte("type: native\nscope: workspace\ncommand: node\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPluginManifest(root); err == nil || !strings.Contains(strings.ToLower(err.Error()), "mystery") {
		t.Fatalf("unknown field err=%v", err)
	}
}
