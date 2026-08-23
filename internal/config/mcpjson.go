package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	return out, nil
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
		// Instruction injection is a process-wide authority. Workspace-local
		// definitions are complete replacements and cannot grant it.
		for name, server := range file.MCPServers {
			server.Trust = false
			server.InjectInstructions = false
			file.MCPServers[name] = server
		}
		files = append(files, file)
	}
	return MergeMCP(files...), nil
}
