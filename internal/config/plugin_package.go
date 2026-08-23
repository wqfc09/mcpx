package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const PluginPackageVersion = 2
const pluginGuidanceSummaryMaxRunes = 240

type pluginPackageIndex struct {
	ManifestVersion int                          `yaml:"manifest_version"`
	Name            string                       `yaml:"name"`
	Description     string                       `yaml:"description,omitempty"`
	Runtime         string                       `yaml:"runtime"`
	Tools           string                       `yaml:"tools,omitempty"`
	Guidance        string                       `yaml:"guidance,omitempty"`
	Requires        *pluginPackageRequires       `yaml:"requires,omitempty"`
	Uses            map[string]pluginPackageUse  `yaml:"uses,omitempty"`
	Watches         []pluginPackageWatch         `yaml:"watches,omitempty"`
	SkillInjection  *pluginPackageSkillInjection `yaml:"skill_injection,omitempty"`
	UI              *pluginPackageUI             `yaml:"ui,omitempty"`
}

type pluginPackageRuntime struct {
	Type    string            `yaml:"type"`
	Scope   string            `yaml:"scope"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

type pluginPackageTools struct {
	Tools []string `yaml:"tools"`
	Inbox string   `yaml:"inbox,omitempty"`
}

type pluginPackageRequires struct {
	Plugins []string `yaml:"plugins,omitempty"`
}

type pluginPackageStringGuard struct {
	Equals string   `yaml:"equals,omitempty"`
	Prefix string   `yaml:"prefix,omitempty"`
	OneOf  []string `yaml:"one_of,omitempty"`
}

type pluginPackageUse struct {
	Plugin      string                              `yaml:"plugin"`
	Tool        string                              `yaml:"tool"`
	Automatic   bool                                `yaml:"automatic,omitempty"`
	Constraints map[string]pluginPackageStringGuard `yaml:"constraints,omitempty"`
}

type pluginPackageWatch struct {
	Plugin string `yaml:"plugin"`
	Source string `yaml:"source"`
	Scope  string `yaml:"scope,omitempty"`
}

type pluginPackageContribution struct {
	Plugin string `yaml:"plugin"`
	Slot   string `yaml:"slot"`
	Path   string `yaml:"path"`
}

type pluginPackageContributionSlot struct {
	Slot     string `yaml:"slot"`
	Skill    string `yaml:"skill,omitempty"`
	MaxBytes int    `yaml:"max_bytes,omitempty"`
}

type pluginPackageSkillInjection struct {
	Accepts  []pluginPackageContributionSlot `yaml:"accepts,omitempty"`
	Provides []pluginPackageContribution     `yaml:"provides,omitempty"`
}

type pluginPackageUI struct {
	TUI *pluginPackageTUI `yaml:"tui,omitempty"`
}

type pluginPackageTUI struct {
	Title   string            `yaml:"title,omitempty"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

type pluginGuidanceFrontMatter struct {
	ID      string `yaml:"id"`
	Summary string `yaml:"summary"`
	Scope   string `yaml:"scope"`
}

func loadPluginPackageV2(manifestPath string) (PluginManifest, error) {
	abs, err := filepath.Abs(strings.TrimSpace(manifestPath))
	if err != nil {
		return PluginManifest{}, err
	}
	var author pluginPackageIndex
	if err := decodeStrictYAML(abs, &author); err != nil {
		return PluginManifest{}, fmt.Errorf("parse Plugin package %s: %w", abs, err)
	}
	if author.ManifestVersion != PluginPackageVersion {
		return PluginManifest{}, fmt.Errorf("Plugin package %s manifest_version must be %d", abs, PluginPackageVersion)
	}
	author.Name = strings.TrimSpace(author.Name)
	if author.Name == "" {
		return PluginManifest{}, fmt.Errorf("Plugin package %s requires name", abs)
	}
	root := filepath.Dir(abs)

	var runtime pluginPackageRuntime
	runtimePath, err := packageAssetPath(root, author.Runtime, "runtime")
	if err != nil {
		return PluginManifest{}, fmt.Errorf("Plugin package %s: %w", abs, err)
	}
	if err := decodeStrictYAML(runtimePath, &runtime); err != nil {
		return PluginManifest{}, fmt.Errorf("parse Plugin runtime %s: %w", runtimePath, err)
	}
	runtime.Type = strings.TrimSpace(runtime.Type)
	runtime.Scope = strings.TrimSpace(runtime.Scope)
	runtime.Command = strings.TrimSpace(runtime.Command)
	if runtime.Type != PluginRuntimeMCP && runtime.Type != PluginRuntimeNative {
		return PluginManifest{}, fmt.Errorf("Plugin package %s runtime.type must be %q or %q", abs, PluginRuntimeMCP, PluginRuntimeNative)
	}
	if runtime.Scope == "" || runtime.Command == "" {
		return PluginManifest{}, fmt.Errorf("Plugin package %s runtime requires scope and command", abs)
	}

	var capabilities *pluginPackageTools
	if strings.TrimSpace(author.Tools) != "" {
		toolsPath, err := packageAssetPath(root, author.Tools, "tools")
		if err != nil {
			return PluginManifest{}, fmt.Errorf("Plugin package %s: %w", abs, err)
		}
		var tools pluginPackageTools
		if err := decodeStrictYAML(toolsPath, &tools); err != nil {
			return PluginManifest{}, fmt.Errorf("parse Plugin tools %s: %w", toolsPath, err)
		}
		capabilities = &tools
	}
	if runtime.Type == PluginRuntimeMCP && capabilities == nil {
		return PluginManifest{}, fmt.Errorf("Plugin package %s MCP runtime requires tools", abs)
	}
	if runtime.Type == PluginRuntimeNative && capabilities != nil {
		return PluginManifest{}, fmt.Errorf("Plugin package %s native runtime cannot declare MCP tools", abs)
	}

	guidance, err := loadPackageGuidance(root, author.Guidance)
	if err != nil {
		return PluginManifest{}, fmt.Errorf("Plugin package %s Guidance: %w", abs, err)
	}
	disabled := false
	plugin := &MCPPlugin{
		Runtime:  runtime.Type,
		Scope:    runtime.Scope,
		Mounts:   map[string]MCPPluginMount{},
		Guidance: guidance,
	}
	if capabilities != nil {
		plugin.Tools = append([]string(nil), capabilities.Tools...)
		plugin.Inbox = capabilities.Inbox
	}
	if author.Requires != nil {
		plugin.Depends = append([]string(nil), author.Requires.Plugins...)
	}
	for alias, use := range author.Uses {
		guards := make(map[string]MCPPluginStringGuard, len(use.Constraints))
		for field, guard := range use.Constraints {
			guards[field] = MCPPluginStringGuard{Equals: guard.Equals, Prefix: guard.Prefix, OneOf: append([]string(nil), guard.OneOf...)}
		}
		plugin.Mounts[alias] = MCPPluginMount{Plugin: use.Plugin, Tool: use.Tool, Automatic: use.Automatic, Guards: guards}
	}
	if len(plugin.Mounts) == 0 {
		plugin.Mounts = nil
	}
	for _, watch := range author.Watches {
		plugin.Subscriptions = append(plugin.Subscriptions, MCPPluginSubscription{Plugin: watch.Plugin, Kind: watch.Source, Scope: watch.Scope})
	}
	if author.SkillInjection != nil {
		for _, slot := range author.SkillInjection.Accepts {
			plugin.Accepts = append(plugin.Accepts, MCPPluginContributionSlot{Slot: slot.Slot, Skill: slot.Skill, MaxBytes: slot.MaxBytes})
		}
		for _, contribution := range author.SkillInjection.Provides {
			plugin.Contributes = append(plugin.Contributes, MCPPluginContribution{Plugin: contribution.Plugin, Slot: contribution.Slot, Path: contribution.Path})
		}
	}
	if author.UI != nil && author.UI.TUI != nil {
		tui := author.UI.TUI
		plugin.TUI = &MCPPluginTUI{Title: tui.Title, Command: tui.Command, Args: append([]string(nil), tui.Args...), Env: cloneManifestEnv(tui.Env)}
	}
	server := MCPServer{
		Type: "stdio", Description: strings.TrimSpace(author.Description),
		Command: runtime.Command, Args: append([]string(nil), runtime.Args...), Env: cloneManifestEnv(runtime.Env),
		Enabled: &disabled, IsPlugin: true, Trust: true, Plugin: plugin,
	}
	resolvePluginManifestPaths(root, &server)
	if err := validatePluginDefinition(author.Name, server); err != nil {
		return PluginManifest{}, fmt.Errorf("validate Plugin package %s: %w", abs, err)
	}
	return PluginManifest{ManifestVersion: author.ManifestVersion, Name: author.Name, Server: server}, nil
}

func loadPackageGuidance(root, value string) ([]MCPPluginGuidance, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	directory, err := packageAssetPath(root, value, "guidance")
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("guidance must reference a directory: %s", directory)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	items := make([]MCPPluginGuidance, 0, len(paths))
	for _, path := range paths {
		metadata, err := readGuidanceFrontMatter(path)
		if err != nil {
			return nil, err
		}
		items = append(items, MCPPluginGuidance{
			ID: metadata.ID, Summary: metadata.Summary, Scope: metadata.Scope,
			Path: path, FrontMatter: true,
		})
	}
	return items, nil
}

func readGuidanceFrontMatter(path string) (pluginGuidanceFrontMatter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginGuidanceFrontMatter{}, err
	}
	if len(data) > PluginGuidanceHardMaxBytes {
		return pluginGuidanceFrontMatter{}, fmt.Errorf("Guidance %s is %d bytes; maximum is %d", path, len(data), PluginGuidanceHardMaxBytes)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return pluginGuidanceFrontMatter{}, fmt.Errorf("Guidance %s requires YAML front matter and a non-empty body", path)
	}
	rest := strings.TrimPrefix(text, "---\n")
	boundary := strings.Index(rest, "\n---\n")
	if boundary < 0 || strings.TrimSpace(rest[boundary+5:]) == "" {
		return pluginGuidanceFrontMatter{}, fmt.Errorf("Guidance %s requires YAML front matter and a non-empty body", path)
	}
	var metadata pluginGuidanceFrontMatter
	if err := decodeStrictYAMLBytes([]byte(strings.TrimSpace(rest[:boundary])), &metadata); err != nil {
		return pluginGuidanceFrontMatter{}, fmt.Errorf("parse Guidance front matter %s: %w", path, err)
	}
	metadata.ID = strings.TrimSpace(metadata.ID)
	metadata.Summary = strings.TrimSpace(metadata.Summary)
	metadata.Scope = strings.TrimSpace(metadata.Scope)
	if metadata.ID == "" || metadata.Summary == "" || metadata.Scope == "" {
		return pluginGuidanceFrontMatter{}, fmt.Errorf("Guidance %s front matter requires id, summary and scope", path)
	}
	if utf8.RuneCountInString(metadata.Summary) > pluginGuidanceSummaryMaxRunes {
		return pluginGuidanceFrontMatter{}, fmt.Errorf("Guidance %s summary exceeds %d characters", path, pluginGuidanceSummaryMaxRunes)
	}
	return metadata, nil
}

func packageAssetPath(root, value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	return filepath.Clean(filepath.Join(root, value)), nil
}

func decodeStrictYAML(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrictYAMLBytes(data, target)
}

func decodeStrictYAMLBytes(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple YAML documents are not allowed")
}
