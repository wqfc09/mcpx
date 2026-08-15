package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// LoadMCPFile reads one .mcp.json; missing => empty servers.
func LoadMCPFile(path string) (MCPFile, error) {
	out := MCPFile{MCPServers: map[string]MCPServer{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("parse %s: %w", path, err)
	}
	if out.MCPServers == nil {
		out.MCPServers = map[string]MCPServer{}
	}
	if err := ValidateMCPFile(out); err != nil {
		return MCPFile{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return out, nil
}

// ValidateMCPFile enforces the current Plugin V1 contract. Plugin tools are
// always explicit; wildcard and implicit inbox mounting are intentionally not
// supported.
func ValidateMCPFile(file MCPFile) error {
	names := make([]string, 0, len(file.MCPServers))
	for name := range file.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := file.MCPServers[name]
		if !server.IsPlugin {
			if server.Plugin != nil {
				return fmt.Errorf("MCP server %q has plugin config but isPlugin is false", name)
			}
			continue
		}
		if server.Plugin == nil {
			return fmt.Errorf("Plugin %q requires plugin config", name)
		}
		if server.Plugin.Tools == nil {
			return fmt.Errorf("Plugin %q requires explicit plugin.tools", name)
		}
		inbox := strings.TrimSpace(server.Plugin.Inbox)
		if inbox == "" {
			return fmt.Errorf("Plugin %q requires plugin.inbox", name)
		}
		if strings.Contains(inbox, "*") {
			return fmt.Errorf("Plugin %q inbox must be an explicit tool name; wildcard is not allowed", name)
		}
		seen := make(map[string]bool, len(server.Plugin.Tools))
		for _, raw := range server.Plugin.Tools {
			tool := strings.TrimSpace(raw)
			switch {
			case tool == "":
				return fmt.Errorf("Plugin %q tools must contain only non-empty explicit names", name)
			case strings.Contains(tool, "*"):
				return fmt.Errorf("Plugin %q tool %q uses a wildcard; wildcard is not allowed", name, raw)
			case tool == inbox:
				return fmt.Errorf("Plugin %q inbox %q cannot also be a mounted public tool", name, inbox)
			case seen[tool]:
				return fmt.Errorf("Plugin %q tool %q is listed more than once", name, tool)
			}
			seen[tool] = true
		}
	}
	return nil
}

// MergeMCP merges files in order by server name; later files win.
func MergeMCP(files ...MCPFile) MCPFile {
	out := MCPFile{MCPServers: map[string]MCPServer{}}
	for _, file := range files {
		for k, v := range file.MCPServers {
			out.MCPServers[k] = v
		}
	}
	return out
}

// LoadMergedMCP loads global then workspace MCP JSON files for a workspace.
func LoadMergedMCP(workspacePath string) (MCPFile, error) {
	gPath, err := GlobalMCPPath()
	if err != nil {
		return MCPFile{}, err
	}
	files := make([]MCPFile, 0, 1+len(ProjectMCPConfigPaths(workspacePath)))
	g, err := LoadMCPFile(gPath)
	if err != nil {
		return MCPFile{}, err
	}
	files = append(files, g)
	for _, path := range ProjectMCPConfigPaths(workspacePath) {
		file, err := LoadMCPFile(path)
		if err != nil {
			return MCPFile{}, err
		}
		// Plugin identity, generic trust, and instruction injection are
		// process-wide authorities. A repository-local definition is complete and
		// never inherits these fields from the global server it replaces.
		for name, server := range file.MCPServers {
			server.IsPlugin = false
			server.Trust = false
			server.InjectInstructions = false
			server.Plugin = nil
			file.MCPServers[name] = server
		}
		files = append(files, file)
	}
	return MergeMCP(files...), nil
}
