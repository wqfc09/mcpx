package config

import (
	"crypto/sha256"
	"encoding/hex"
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

// ValidateMCPFile validates the generic MCP file shape. Plugin-specific
// author/runtime contracts are layered on by the Plugin package subsystem.
func ValidateMCPFile(file MCPFile) error {
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

// LoadMergedMCP loads Global definitions plus one Workspace overlay. A
// Workspace may define a new ordinary MCP registration. When a name already
// exists Global, the Workspace entry is activation-only: it may only override
// enabled and can never redefine command, trust, or instruction authority.
func LoadMergedMCP(workspacePath string) (MCPFile, error) {
	gPath, err := GlobalMCPPath()
	if err != nil {
		return MCPFile{}, err
	}
	global, err := LoadMCPFile(gPath)
	if err != nil {
		return MCPFile{}, err
	}
	for name, server := range global.MCPServers {
		server.Source = MCPSourceGlobal
		global.MCPServers[name] = server
	}

	workspaceFile, err := LoadMCPFile(ProjectMCPPath(workspacePath))
	if err != nil {
		return MCPFile{}, err
	}
	if err := validateWorkspaceMCPFile(workspaceFile, global); err != nil {
		return MCPFile{}, fmt.Errorf("validate %s: %w", ProjectMCPPath(workspacePath), err)
	}
	merged := MergeMCP(global)
	for name, server := range workspaceFile.MCPServers {
		if globalServer, exists := global.MCPServers[name]; exists {
			// Same-name Workspace entries are activation-only. Preserve the full
			// Global authority/definition and replace only the enabled switch.
			effective := globalServer
			effective.Enabled = server.Enabled
			merged.MCPServers[name] = effective
			continue
		}
		server.Source = MCPSourceWorkspace
		server.TrustRequested = server.Trust
		server.Trust = false
		server.TrustFingerprint = MCPRegistrationFingerprint(server)
		merged.MCPServers[name] = server
	}
	return merged, nil
}

func validateWorkspaceMCPFile(file MCPFile, global MCPFile) error {
	names := make([]string, 0, len(file.MCPServers))
	for name := range file.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := file.MCPServers[name]
		if globalServer, exists := global.MCPServers[name]; exists {
			_ = globalServer
			if !workspaceActivationOnly(server) {
				return fmt.Errorf("Workspace MCP server %q matches a Global registration; only enabled may be overridden", name)
			}
			continue
		}
	}
	return nil
}

func workspaceActivationOnly(server MCPServer) bool {
	return server.Enabled != nil && strings.TrimSpace(server.Type) == "" && strings.TrimSpace(server.Description) == "" && strings.TrimSpace(server.Command) == "" && len(server.Args) == 0 && len(server.Env) == 0 && !server.Trust && !server.InjectInstructions
}

// MCPRegistrationFingerprint identifies the executable/capability contract that
// a Workspace trust approval covers. enabled, trust, description, and env are
// intentionally excluded from this revision.
func MCPRegistrationFingerprint(server MCPServer) string {
	type fingerprint struct {
		Type               string   `json:"type"`
		Command            string   `json:"command"`
		Args               []string `json:"args,omitempty"`
		InjectInstructions bool     `json:"injectInstructions,omitempty"`
	}
	typeName := strings.TrimSpace(server.Type)
	if typeName == "" {
		typeName = "stdio"
	}
	payload := fingerprint{Type: typeName, Command: strings.TrimSpace(server.Command), Args: append([]string(nil), server.Args...), InjectInstructions: server.InjectInstructions}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}
