package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcpx/internal/config"
	"mcpx/internal/workspace"
)

const (
	pluginContributionsEnv         = "MCPX_PLUGIN_CONTRIBUTIONS_FILE"
	pluginContributionsRevisionEnv = "MCPX_PLUGIN_CONTRIBUTIONS_REVISION"
)

type resolvedPluginContribution struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Slot     string `json:"slot"`
	Revision string `json:"revision"`
	Content  string `json:"content"`
}

type resolvedPluginContributionFile struct {
	Version       int                          `json:"version"`
	Target        string                       `json:"target"`
	Revision      string                       `json:"revision"`
	Contributions []resolvedPluginContribution `json:"contributions"`
}

func (r *Runtime) prepareMCPPluginServer(ws workspace.Workspace, targetName string, server config.MCPServer) (config.MCPServer, error) {
	if server.Plugin == nil || server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
		return server, nil
	}
	resolved, revision, err := r.resolvePluginContributions(ws, targetName, server)
	if err != nil {
		return config.MCPServer{}, err
	}
	if len(resolved) == 0 {
		return server, nil
	}
	runtimeDir := r.pluginLeases.runtimeDir(targetName, server.Plugin.RuntimeScope(), ws)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return config.MCPServer{}, fmt.Errorf("prepare Plugin contribution runtime: %w", err)
	}
	manifest := resolvedPluginContributionFile{Version: 1, Target: targetName, Revision: revision, Contributions: resolved}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return config.MCPServer{}, fmt.Errorf("encode Plugin contributions: %w", err)
	}
	path := filepath.Join(runtimeDir, "contributions.json")
	tmp, err := os.CreateTemp(runtimeDir, ".contributions-*.tmp")
	if err != nil {
		return config.MCPServer{}, fmt.Errorf("create Plugin contribution manifest temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return config.MCPServer{}, fmt.Errorf("protect Plugin contribution manifest temp file: %w", err)
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return config.MCPServer{}, fmt.Errorf("write Plugin contribution manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return config.MCPServer{}, fmt.Errorf("close Plugin contribution manifest: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return config.MCPServer{}, fmt.Errorf("publish Plugin contribution manifest: %w", err)
	}
	copy := server
	copy.RuntimeEnv = cloneStringMap(server.RuntimeEnv)
	copy.RuntimeEnv[pluginContributionsEnv] = path
	copy.RuntimeEnv[pluginContributionsRevisionEnv] = revision
	return copy, nil
}

func (r *Runtime) resolvePluginContributions(ws workspace.Workspace, targetName string, target config.MCPServer) ([]resolvedPluginContribution, string, error) {
	if strings.TrimSpace(ws.Path) == "" || target.Plugin == nil || len(target.Plugin.Accepts) == 0 {
		return nil, "", nil
	}
	merged, err := config.LoadMergedMCP(ws.Path)
	if err != nil {
		return nil, "", err
	}
	limits := map[string]int{}
	for _, slot := range target.Plugin.Accepts {
		limits[strings.TrimSpace(slot.Slot)] = slot.EffectiveMaxBytes()
	}
	sources := make([]string, 0, len(merged.MCPServers))
	for name := range merged.MCPServers {
		sources = append(sources, name)
	}
	sort.Strings(sources)
	resolved := []resolvedPluginContribution{}
	for _, sourceName := range sources {
		if sourceName == targetName {
			continue
		}
		source := merged.MCPServers[sourceName]
		if !source.IsPlugin || !source.IsEnabled() || source.Plugin == nil || source.Plugin.RuntimeType() != config.PluginRuntimeController {
			continue
		}
		for _, contribution := range source.Plugin.Contributes {
			if strings.TrimSpace(contribution.Plugin) != targetName {
				continue
			}
			slot := strings.TrimSpace(contribution.Slot)
			limit, ok := limits[slot]
			if !ok {
				return nil, "", fmt.Errorf("Plugin %q does not accept contribution slot %q", targetName, slot)
			}
			body, err := os.ReadFile(contribution.Path)
			if err != nil {
				return nil, "", fmt.Errorf("read Plugin %q contribution %q: %w", sourceName, contribution.Path, err)
			}
			if len(body) > limit || len(body) > config.PluginContributionHardMaxBytes {
				return nil, "", fmt.Errorf("Plugin %q contribution for %s/%s is %d bytes; limit is %d", sourceName, targetName, slot, len(body), limit)
			}
			digest := sha256.Sum256(body)
			resolved = append(resolved, resolvedPluginContribution{
				Source: sourceName, Target: targetName, Slot: slot,
				Revision: "sha256:" + hex.EncodeToString(digest[:]), Content: string(body),
			})
		}
	}
	if len(resolved) == 0 {
		return nil, "", nil
	}
	sort.Slice(resolved, func(i, j int) bool {
		if resolved[i].Source != resolved[j].Source {
			return resolved[i].Source < resolved[j].Source
		}
		if resolved[i].Slot != resolved[j].Slot {
			return resolved[i].Slot < resolved[j].Slot
		}
		return resolved[i].Revision < resolved[j].Revision
	})
	encoded, _ := json.Marshal(resolved)
	digest := sha256.Sum256(encoded)
	return resolved, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneStringMap(source map[string]string) map[string]string {
	copy := map[string]string{}
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
