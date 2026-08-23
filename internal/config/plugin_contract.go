package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func validatePluginDefinition(name string, server MCPServer) error {
	if !server.IsPlugin {
		if server.Plugin != nil {
			return fmt.Errorf("MCP server %q has plugin config but isPlugin is false", name)
		}
		return nil
	}
	if server.Plugin == nil {
		return fmt.Errorf("Plugin %q requires plugin config", name)
	}
	plugin := server.Plugin
	runtimeType := plugin.RuntimeType()
	if runtimeType != PluginRuntimeMCP && runtimeType != PluginRuntimeNative {
		return fmt.Errorf("Plugin %q runtime must be %q or %q", name, PluginRuntimeMCP, PluginRuntimeNative)
	}
	scope := plugin.RuntimeScope()
	if scope != PluginScopeInstance && scope != PluginScopeWorkspace {
		return fmt.Errorf("Plugin %q scope must be %q or %q", name, PluginScopeInstance, PluginScopeWorkspace)
	}
	if runtimeType == PluginRuntimeNative && scope != PluginScopeWorkspace {
		return fmt.Errorf("Native Plugin %q must use workspace scope", name)
	}
	if strings.TrimSpace(server.Command) == "" {
		return fmt.Errorf("Plugin %q requires command", name)
	}
	if plugin.TUI != nil {
		if strings.TrimSpace(plugin.TUI.Command) == "" {
			return fmt.Errorf("Plugin %q tui requires command", name)
		}
		for key := range plugin.TUI.Env {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("Plugin %q tui env keys must be non-empty", name)
			}
		}
	}

	seenGuidance := map[string]bool{}
	for _, guidance := range plugin.Guidance {
		id := strings.TrimSpace(guidance.ID)
		if id == "" {
			return fmt.Errorf("Plugin %q guidance id cannot be empty", name)
		}
		if seenGuidance[id] {
			return fmt.Errorf("Plugin %q guidance id %q is listed more than once", name, guidance.ID)
		}
		seenGuidance[id] = true
		scope := strings.TrimSpace(guidance.Scope)
		if scope != PluginGuidanceScopeAttachment && scope != PluginGuidanceScopeContext {
			return fmt.Errorf("Plugin %q guidance %q scope must be %q or %q", name, guidance.ID, PluginGuidanceScopeAttachment, PluginGuidanceScopeContext)
		}
		path := strings.TrimSpace(guidance.Path)
		if path == "" {
			return fmt.Errorf("Plugin %q guidance %q requires path", name, guidance.ID)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("Plugin %q guidance %q path must be absolute: %q", name, guidance.ID, guidance.Path)
		}
	}

	seenDeps := map[string]bool{}
	for _, raw := range plugin.Depends {
		dep := strings.TrimSpace(raw)
		if dep == "" {
			return fmt.Errorf("Plugin %q requires.plugins must contain only non-empty names", name)
		}
		if dep == name {
			return fmt.Errorf("Plugin %q cannot require itself", name)
		}
		if seenDeps[dep] {
			return fmt.Errorf("Plugin %q required Plugin %q is listed more than once", name, dep)
		}
		seenDeps[dep] = true
	}

	for _, injection := range plugin.Contributes {
		if strings.TrimSpace(injection.Plugin) == "" || strings.TrimSpace(injection.Slot) == "" || strings.TrimSpace(injection.Path) == "" {
			return fmt.Errorf("Plugin %q skill_injection.provides entry requires plugin, slot and path", name)
		}
		if injection.Plugin == name {
			return fmt.Errorf("Plugin %q cannot provide Skill Injection to itself", name)
		}
		if !filepath.IsAbs(injection.Path) {
			return fmt.Errorf("Plugin %q Skill Injection path must be absolute: %q", name, injection.Path)
		}
	}

	switch runtimeType {
	case PluginRuntimeMCP:
		if plugin.Tools == nil {
			return fmt.Errorf("MCP Plugin %q requires explicit capabilities.tools", name)
		}
		inbox := strings.TrimSpace(plugin.Inbox)
		if inbox == "" {
			return fmt.Errorf("MCP Plugin %q requires capabilities.inbox", name)
		}
		if strings.Contains(inbox, "*") {
			return fmt.Errorf("MCP Plugin %q inbox must be an explicit Tool name; wildcard is not allowed", name)
		}
		seen := make(map[string]bool, len(plugin.Tools))
		for _, raw := range plugin.Tools {
			tool := strings.TrimSpace(raw)
			switch {
			case tool == "":
				return fmt.Errorf("MCP Plugin %q capabilities.tools must contain only non-empty explicit names", name)
			case strings.Contains(tool, "*"):
				return fmt.Errorf("MCP Plugin %q Tool %q uses a wildcard; wildcard is not allowed", name, raw)
			case tool == inbox:
				return fmt.Errorf("MCP Plugin %q inbox %q cannot also be a public Tool", name, inbox)
			case seen[tool]:
				return fmt.Errorf("MCP Plugin %q Tool %q is listed more than once", name, tool)
			}
			seen[tool] = true
		}
		if len(plugin.Mounts) != 0 || len(plugin.Subscriptions) != 0 {
			return fmt.Errorf("MCP Plugin %q cannot declare host-mediated uses/watches", name)
		}
	case PluginRuntimeNative:
		if len(plugin.Tools) != 0 || strings.TrimSpace(plugin.Inbox) != "" {
			return fmt.Errorf("Native Plugin %q uses the MCPX-hosted Inbox and cannot declare MCP capabilities", name)
		}
		if len(plugin.Accepts) != 0 {
			return fmt.Errorf("Native Plugin %q cannot accept Skill Injection", name)
		}
		for alias, use := range plugin.Mounts {
			if strings.TrimSpace(alias) == "" || strings.TrimSpace(use.Plugin) == "" || strings.TrimSpace(use.Tool) == "" {
				return fmt.Errorf("Native Plugin %q use %q requires plugin and tool", name, alias)
			}
			if use.Plugin == name {
				return fmt.Errorf("Native Plugin %q use %q cannot target itself", name, alias)
			}
			if len(use.Guards) > 0 && !use.Automatic {
				return fmt.Errorf("Native Plugin %q use %q constraints require automatic=true", name, alias)
			}
			for argument, constraint := range use.Guards {
				if strings.TrimSpace(argument) == "" {
					return fmt.Errorf("Native Plugin %q use %q constraint argument cannot be empty", name, alias)
				}
				rules := 0
				if constraint.Equals != "" {
					rules++
				}
				if constraint.Prefix != "" {
					rules++
				}
				if len(constraint.OneOf) > 0 {
					rules++
					seen := map[string]bool{}
					for _, value := range constraint.OneOf {
						if strings.TrimSpace(value) == "" || seen[value] {
							return fmt.Errorf("Native Plugin %q use %q constraint %q one_of values must be non-empty and unique", name, alias, argument)
						}
						seen[value] = true
					}
				}
				if rules != 1 {
					return fmt.Errorf("Native Plugin %q use %q constraint %q must declare exactly one of equals/prefix/one_of", name, alias, argument)
				}
			}
		}
		for _, watch := range plugin.Subscriptions {
			if strings.TrimSpace(watch.Plugin) == "" || strings.TrimSpace(watch.Kind) != PluginSubscriptionInbox {
				return fmt.Errorf("Native Plugin %q watches must use source=%q", name, PluginSubscriptionInbox)
			}
			if watch.Plugin == name {
				return fmt.Errorf("Native Plugin %q cannot watch itself", name)
			}
			scope := strings.TrimSpace(watch.Scope)
			if scope == "" {
				scope = PluginSubscriptionScopeWorkspace
			}
			if scope != PluginSubscriptionScopeWorkspace && scope != PluginSubscriptionScopeSessions {
				return fmt.Errorf("Native Plugin %q watch scope must be %q or %q", name, PluginSubscriptionScopeWorkspace, PluginSubscriptionScopeSessions)
			}
		}
	}

	seenSlots := map[string]bool{}
	for _, slot := range plugin.Accepts {
		slotName := strings.TrimSpace(slot.Slot)
		if slotName == "" {
			return fmt.Errorf("Plugin %q Skill Injection accept slot cannot be empty", name)
		}
		if seenSlots[slotName] {
			return fmt.Errorf("Plugin %q Skill Injection slot %q is listed more than once", name, slot.Slot)
		}
		seenSlots[slotName] = true
		if slot.MaxBytes < 0 || slot.EffectiveMaxBytes() > PluginContributionHardMaxBytes {
			return fmt.Errorf("Plugin %q Skill Injection slot %q max_bytes must be between 1 and %d when set", name, slot.Slot, PluginContributionHardMaxBytes)
		}
	}
	return nil
}

func validatePluginGraph(file MCPFile) error {
	plugins := map[string]MCPServer{}
	guidanceProviders := map[string]string{}
	for name, server := range file.MCPServers {
		if server.IsPlugin {
			plugins[name] = server
			if server.Plugin != nil {
				for _, guidance := range server.Plugin.Guidance {
					id := strings.TrimSpace(guidance.ID)
					if prior := guidanceProviders[id]; prior != "" && prior != name {
						return fmt.Errorf("Plugin guidance id %q is declared by both %q and %q", id, prior, name)
					}
					guidanceProviders[id] = name
				}
			}
		}
	}
	for name, server := range plugins {
		plugin := server.Plugin
		declaredDeps := map[string]bool{}
		for _, raw := range plugin.Depends {
			declaredDeps[strings.TrimSpace(raw)] = true
		}
		for _, raw := range plugin.Depends {
			dep := strings.TrimSpace(raw)
			if _, ok := plugins[dep]; !ok {
				return fmt.Errorf("Plugin %q requires unknown Plugin %q", name, dep)
			}
		}
		for alias, use := range plugin.Mounts {
			if !declaredDeps[strings.TrimSpace(use.Plugin)] {
				return fmt.Errorf("Native Plugin %q use %q target %q must be declared in requires.plugins", name, alias, use.Plugin)
			}
			target, ok := plugins[use.Plugin]
			if !ok {
				return fmt.Errorf("Native Plugin %q use %q targets unknown Plugin %q", name, alias, use.Plugin)
			}
			if target.Plugin.RuntimeType() != PluginRuntimeMCP {
				return fmt.Errorf("Native Plugin %q use %q must target an MCP Plugin", name, alias)
			}
			found := false
			for _, tool := range target.Plugin.Tools {
				if strings.TrimSpace(tool) == strings.TrimSpace(use.Tool) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("Native Plugin %q use %q targets unavailable Tool %q on Plugin %q", name, alias, use.Tool, use.Plugin)
			}
		}
		for _, watch := range plugin.Subscriptions {
			if !declaredDeps[strings.TrimSpace(watch.Plugin)] {
				return fmt.Errorf("Native Plugin %q watch target %q must be declared in requires.plugins", name, watch.Plugin)
			}
			target, ok := plugins[watch.Plugin]
			if !ok {
				return fmt.Errorf("Native Plugin %q watch targets unknown Plugin %q", name, watch.Plugin)
			}
			if target.Plugin.RuntimeType() != PluginRuntimeMCP || strings.TrimSpace(target.Plugin.Inbox) == "" {
				return fmt.Errorf("Native Plugin %q watch target %q has no MCP Inbox", name, watch.Plugin)
			}
		}
		for _, injection := range plugin.Contributes {
			if !declaredDeps[strings.TrimSpace(injection.Plugin)] {
				return fmt.Errorf("Plugin %q Skill Injection target %q must be declared in requires.plugins", name, injection.Plugin)
			}
			target, ok := plugins[injection.Plugin]
			if !ok {
				return fmt.Errorf("Plugin %q Skill Injection targets unknown Plugin %q", name, injection.Plugin)
			}
			if target.Plugin.RuntimeType() != PluginRuntimeMCP || target.Plugin.RuntimeScope() != PluginScopeWorkspace {
				return fmt.Errorf("Plugin %q Skill Injection target %q must be a workspace-scoped MCP Plugin", name, injection.Plugin)
			}
			accepted := false
			for _, slot := range target.Plugin.Accepts {
				if strings.TrimSpace(slot.Slot) == strings.TrimSpace(injection.Slot) {
					accepted = true
					break
				}
			}
			if !accepted {
				return fmt.Errorf("Plugin %q Skill Injection slot %q is not accepted by Plugin %q", name, injection.Slot, injection.Plugin)
			}
		}
	}

	state := map[string]uint8{}
	var visit func(string, []string) error
	visit = func(name string, stack []string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("Plugin dependency cycle: %s -> %s", strings.Join(stack, " -> "), name)
		case 2:
			return nil
		}
		state[name] = 1
		deps := append([]string(nil), plugins[name].Plugin.Depends...)
		sort.Strings(deps)
		for _, dep := range deps {
			if err := visit(strings.TrimSpace(dep), append(stack, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	names := make([]string, 0, len(plugins))
	for name := range plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return err
		}
	}
	return nil
}
