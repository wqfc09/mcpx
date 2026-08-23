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

// LoadMCPFile reads one self-contained .mcp.json; missing => empty servers.
// Standalone files must contain a complete valid Plugin dependency graph.
func LoadMCPFile(path string) (MCPFile, error) {
	return loadMCPFile(path, true)
}

// loadMCPFileDefinitions permits a Workspace file to reference Plugin
// dependencies supplied by the Global file. The complete merged graph is
// validated after overlay resolution in LoadMergedMCPFrom.
func loadMCPFileDefinitions(path string) (MCPFile, error) {
	return loadMCPFile(path, false)
}

// LoadWorkspaceMCPFile reads the repo-level MCP configuration without requiring
// its Plugin dependency graph to be self-contained. Callers must validate it
// against Global definitions with ValidateWorkspaceMCPFile before persisting.
func LoadWorkspaceMCPFile(workspacePath string) (MCPFile, error) {
	return loadMCPFileDefinitions(ProjectMCPPath(workspacePath))
}

func loadMCPFile(path string, validateGraph bool) (MCPFile, error) {
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
	var validateErr error
	if validateGraph {
		validateErr = ValidateMCPFile(out)
	} else {
		validateErr = validateMCPDefinitions(out)
	}
	if validateErr != nil {
		return MCPFile{}, fmt.Errorf("validate %s: %w", path, validateErr)
	}
	return out, nil
}

// ValidateMCPFile enforces the Plugin contract and resolves the complete
// dependency/capability graph for one effective MCP configuration.
func ValidateMCPFile(file MCPFile) error {
	if err := validateMCPDefinitions(file); err != nil {
		return err
	}
	return validatePluginGraph(file)
}

func validateMCPDefinitions(file MCPFile) error {
	names := make([]string, 0, len(file.MCPServers))
	for name := range file.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validatePluginDefinition(name, file.MCPServers[name]); err != nil {
			return err
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

// LoadMergedMCP loads Global definitions plus one Workspace configuration.
// A Workspace may define ordinary MCP registrations and complete Plugin
// definitions. A same-name Global Plugin may be explicitly replaced by a full
// Workspace Plugin definition; an enabled-only entry remains a lightweight
// activation overlay. Ordinary same-name Global MCP registrations remain
// activation-only and cannot be redefined by a Workspace.
func LoadMergedMCP(workspacePath string) (MCPFile, error) {
	gPath, err := GlobalMCPPath()
	if err != nil {
		return MCPFile{}, err
	}
	return LoadMergedMCPFrom(gPath, workspacePath)
}

// LoadMergedMCPFrom applies one Workspace configuration to an explicit Global
// MCP file. Local administration commands use this when a running MCPX
// Instance owns a different Home than the caller's MCPX_HOME environment.
func LoadMergedMCPFrom(globalMCPPath, workspacePath string) (MCPFile, error) {
	global, err := LoadMCPFile(globalMCPPath)
	if err != nil {
		return MCPFile{}, err
	}
	for name, server := range global.MCPServers {
		server.Source = MCPSourceGlobal
		global.MCPServers[name] = server
	}

	workspaceFile, err := loadMCPFileDefinitions(ProjectMCPPath(workspacePath))
	if err != nil {
		return MCPFile{}, err
	}
	merged, err := mergeWorkspaceMCP(global, workspaceFile, true)
	if err != nil {
		return MCPFile{}, fmt.Errorf("validate %s: %w", ProjectMCPPath(workspacePath), err)
	}
	return merged, nil
}

// ValidateWorkspaceMCPFile validates a Workspace MCP configuration against
// explicit Global definitions. Administration/bootstrap commands use this
// before persisting changes so an invalid effective Plugin graph is never
// written first.
func ValidateWorkspaceMCPFile(file MCPFile, global MCPFile) error {
	if err := validateMCPDefinitions(file); err != nil {
		return err
	}
	_, err := mergeWorkspaceMCP(global, file, false)
	return err
}

func mergeWorkspaceMCP(global, workspace MCPFile, decorate bool) (MCPFile, error) {
	merged := MergeMCP(global)
	names := make([]string, 0, len(workspace.MCPServers))
	for name := range workspace.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := workspace.MCPServers[name]
		globalServer, exists := global.MCPServers[name]
		if exists && workspaceActivationOnly(server) {
			effective := globalServer
			effective.Enabled = server.Enabled
			merged.MCPServers[name] = effective
			continue
		}
		if exists && (!server.IsPlugin || server.Plugin == nil || !globalServer.IsPlugin) {
			return MCPFile{}, fmt.Errorf("Workspace MCP server %q matches a Global registration; only enabled may be overridden unless both definitions are Plugins", name)
		}
		if server.IsPlugin && server.Plugin != nil && !server.Trust {
			return MCPFile{}, fmt.Errorf("Workspace Plugin %q requires trust=true", name)
		}
		if decorate {
			server.Source = MCPSourceWorkspace
			if server.IsPlugin {
				// Explicit Workspace Plugin definitions are trusted Plugin
				// registrations. This mirrors Plugin install semantics without
				// requiring a user-global trust record under ~/.mcpx.
				server.TrustRequested = false
				server.TrustFingerprint = ""
			} else {
				server.TrustRequested = server.Trust
				server.Trust = false
				server.TrustFingerprint = MCPRegistrationFingerprint(server)
			}
		}
		merged.MCPServers[name] = server
	}
	if err := ValidateMCPFile(merged); err != nil {
		return MCPFile{}, err
	}
	return merged, nil
}

func workspaceActivationOnly(server MCPServer) bool {
	return server.Enabled != nil && strings.TrimSpace(server.Type) == "" && strings.TrimSpace(server.Description) == "" && strings.TrimSpace(server.Command) == "" && len(server.Args) == 0 && len(server.Env) == 0 && !server.IsPlugin && !server.Trust && !server.InjectInstructions && server.Plugin == nil
}

// MCPRegistrationFingerprint identifies the executable/capability contract that
// a Workspace trust approval covers. enabled, trust, description, and env are
// intentionally excluded from this revision.
func MCPRegistrationFingerprint(server MCPServer) string {
	type pluginFingerprint struct {
		Runtime       string                      `json:"runtime,omitempty"`
		Scope         string                      `json:"scope,omitempty"`
		Tools         []string                    `json:"tools,omitempty"`
		Inbox         string                      `json:"inbox,omitempty"`
		Depends       []string                    `json:"depends,omitempty"`
		Mounts        map[string]MCPPluginMount   `json:"mounts,omitempty"`
		Subscriptions []MCPPluginSubscription     `json:"subscriptions,omitempty"`
		Contributes   []MCPPluginContribution     `json:"contributes,omitempty"`
		Accepts       []MCPPluginContributionSlot `json:"accepts,omitempty"`
		Guidance      []MCPPluginGuidance         `json:"guidance,omitempty"`
		TUI           *MCPPluginTUI               `json:"tui,omitempty"`
	}
	type fingerprint struct {
		Type               string             `json:"type"`
		Command            string             `json:"command"`
		Args               []string           `json:"args,omitempty"`
		IsPlugin           bool               `json:"isPlugin,omitempty"`
		InjectInstructions bool               `json:"injectInstructions,omitempty"`
		Plugin             *pluginFingerprint `json:"plugin,omitempty"`
	}
	typeName := strings.TrimSpace(server.Type)
	if typeName == "" {
		typeName = "stdio"
	}
	payload := fingerprint{Type: typeName, Command: strings.TrimSpace(server.Command), Args: append([]string(nil), server.Args...), IsPlugin: server.IsPlugin, InjectInstructions: server.InjectInstructions}
	if server.Plugin != nil {
		tools := append([]string(nil), server.Plugin.Tools...)
		for i := range tools {
			tools[i] = strings.TrimSpace(tools[i])
		}
		sort.Strings(tools)
		depends := append([]string(nil), server.Plugin.Depends...)
		for i := range depends {
			depends[i] = strings.TrimSpace(depends[i])
		}
		sort.Strings(depends)
		payload.Plugin = &pluginFingerprint{
			Runtime: server.Plugin.RuntimeType(), Scope: server.Plugin.RuntimeScope(), Tools: tools,
			Inbox: strings.TrimSpace(server.Plugin.Inbox), Depends: depends, Mounts: server.Plugin.Mounts,
			Subscriptions: append([]MCPPluginSubscription(nil), server.Plugin.Subscriptions...),
			Contributes:   append([]MCPPluginContribution(nil), server.Plugin.Contributes...),
			Accepts:       append([]MCPPluginContributionSlot(nil), server.Plugin.Accepts...),
			Guidance:      append([]MCPPluginGuidance(nil), server.Plugin.Guidance...),
			TUI:           server.Plugin.TUI,
		}
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// PluginDefinitionRevision fingerprints the complete persisted Plugin
// definition. It is for administration/status and changes when human-facing
// metadata or a TUI contribution changes.
func PluginDefinitionRevision(server MCPServer) string {
	encoded, _ := json.Marshal(server)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// PluginRuntimeRevision fingerprints only state that can affect the managed
// Plugin business runtime. Activation, trust, description and native TUI page
// definitions are intentionally excluded, so updating a monitor/dashboard does
// not kill a running JEA/Comet process.
func PluginRuntimeRevision(server MCPServer) string {
	copyServer := server
	copyServer.Enabled = nil
	copyServer.Description = ""
	copyServer.Trust = false
	if server.Plugin != nil {
		copyPlugin := *server.Plugin
		copyPlugin.TUI = nil
		copyPlugin.Guidance = nil
		copyServer.Plugin = &copyPlugin
	}
	encoded, _ := json.Marshal(copyServer)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// PluginRuntimeGraphRevision fingerprints one Plugin runtime contract plus the
// recursive runtime contracts of its declared dependencies. Controller leases
// use this so a dependency runtime change replaces the coordinating process,
// while description/TUI-only changes remain non-disruptive.
func PluginRuntimeGraphRevision(file MCPFile, pluginName string) (string, error) {
	parts := make([]string, 0, 4)
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		name = strings.TrimSpace(name)
		server, ok := file.MCPServers[name]
		if !ok || !server.IsPlugin || server.Plugin == nil {
			return fmt.Errorf("Plugin runtime graph dependency %q is not a registered Plugin", name)
		}
		if visiting[name] {
			return fmt.Errorf("Plugin runtime graph cycle includes %q", name)
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		parts = append(parts, name+"="+PluginRuntimeRevision(server))
		dependencies := append([]string(nil), server.Plugin.Depends...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if err := visit(strings.TrimSpace(dependency)); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		return nil
	}
	if err := visit(pluginName); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
