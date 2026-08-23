package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadPluginManifestRejectsLegacyV1Entrypoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcpx-plugin.json")
	if err := os.WriteFile(path, []byte(`{"manifest_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPluginManifest(path); err == nil || !strings.Contains(err.Error(), "plugin.yaml") {
		t.Fatalf("legacy manifest must be rejected, err=%v", err)
	}
}

func TestBundledOhMyMCPXPackagesFormWorkspacePluginGraph(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot resolve source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	manifestPaths := []string{
		filepath.Join(root, "plugins", "JEA", "plugin.yaml"),
		filepath.Join(root, "plugins", "comet-mcp", "plugin.yaml"),
		filepath.Join(root, "plugins", "comet-coordinator", "plugin.yaml"),
	}
	for _, path := range manifestPaths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Skip("oh-my-mcpx sibling Plugin packages are not present in this checkout")
		} else if err != nil {
			t.Fatal(err)
		}
	}

	enabled := true
	project := MCPFile{MCPServers: map[string]MCPServer{}}
	for _, path := range manifestPaths {
		manifest, err := LoadPluginManifest(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		server := manifest.Server
		server.Enabled = &enabled
		project.MCPServers[manifest.Name] = server
	}
	workspace := t.TempDir()
	if err := WriteMCPFile(ProjectMCPPath(workspace), project); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadMergedMCPFrom(filepath.Join(t.TempDir(), ".mcp.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"JEA", "Comet", "CometCoordinator"} {
		server, ok := merged.MCPServers[name]
		if !ok || !server.IsPlugin || !server.IsEnabled() || !server.Trust || server.Source != MCPSourceWorkspace || server.Plugin == nil || server.Plugin.TUI == nil {
			t.Fatalf("bundled Workspace Plugin %s=%+v", name, server)
		}
	}
	coordinator := merged.MCPServers["CometCoordinator"]
	if coordinator.Plugin.RuntimeType() != PluginRuntimeNative || len(coordinator.Plugin.Depends) != 2 || coordinator.Plugin.Depends[0] != "Comet" || coordinator.Plugin.Depends[1] != "JEA" {
		t.Fatalf("Coordinator normalized definition=%+v", coordinator.Plugin)
	}
	if len(coordinator.Plugin.Guidance) != 5 {
		t.Fatalf("Coordinator Guidance=%+v", coordinator.Plugin.Guidance)
	}
}
