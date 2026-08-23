package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PluginManifest is the normalized installation unit returned by the Package
// V2 loader. Server is an internal MCPX definition; authors never write this
// shape directly.
type PluginManifest struct {
	ManifestVersion int       `json:"manifest_version"`
	Name            string    `json:"name"`
	Server          MCPServer `json:"server"`
}

// LoadPluginManifest accepts a Package V2 directory or its canonical
// plugin.yaml entrypoint. Legacy single-file mcpx-plugin.json manifests are not
// part of the author contract anymore.
func LoadPluginManifest(path string) (PluginManifest, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return PluginManifest{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return PluginManifest{}, err
	}
	if info.IsDir() {
		return loadPluginPackageV2(filepath.Join(abs, "plugin.yaml"))
	}
	if filepath.Base(abs) != "plugin.yaml" {
		return PluginManifest{}, fmt.Errorf("Plugin package entry must be plugin.yaml or a package directory: %s", abs)
	}
	return loadPluginPackageV2(abs)
}

func cloneManifestEnv(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func resolvePluginManifestPaths(base string, server *MCPServer) {
	if server == nil {
		return
	}
	server.Command = resolveManifestPath(base, server.Command)
	for index, value := range server.Args {
		server.Args[index] = resolveManifestPath(base, value)
	}
	if server.Plugin == nil {
		return
	}
	for index := range server.Plugin.Contributes {
		server.Plugin.Contributes[index].Path = resolveManifestPath(base, server.Plugin.Contributes[index].Path)
	}
	for index := range server.Plugin.Guidance {
		server.Plugin.Guidance[index].Path = resolveManifestPath(base, server.Plugin.Guidance[index].Path)
	}
	if server.Plugin.TUI != nil {
		server.Plugin.TUI.Command = resolveManifestPath(base, server.Plugin.TUI.Command)
		for index, value := range server.Plugin.TUI.Args {
			server.Plugin.TUI.Args[index] = resolveManifestPath(base, value)
		}
	}
}

func resolveManifestPath(base, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || filepath.IsAbs(trimmed) {
		return value
	}
	if trimmed != "." && !strings.HasPrefix(trimmed, "./") && !strings.HasPrefix(trimmed, "../") {
		return value
	}
	return filepath.Clean(filepath.Join(base, trimmed))
}
