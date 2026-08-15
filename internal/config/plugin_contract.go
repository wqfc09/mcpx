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
	if runtimeType != PluginRuntimeMCP && runtimeType != PluginRuntimeController {
		return fmt.Errorf("Plugin %q runtime must be %q or %q", name, PluginRuntimeMCP, PluginRuntimeController)
	}
	scope := plugin.RuntimeScope()
	if scope != PluginScopeInstance && scope != PluginScopeWorkspace {
		return fmt.Errorf("Plugin %q scope must be %q or %q", name, PluginScopeInstance, PluginScopeWorkspace)
	}
	if runtimeType == PluginRuntimeController && scope != PluginScopeWorkspace {
		return fmt.Errorf("Controller Plugin %q must use workspace scope", name)
	}
	if strings.TrimSpace(server.Command) == "" {
		return fmt.Errorf("Plugin %q requires command", name)
	}

	seenDeps := map[string]bool{}
	for _, raw := range plugin.Depends {
		dep := strings.TrimSpace(raw)
		if dep == "" {
			return fmt.Errorf("Plugin %q dependencies must be non-empty", name)
		}
		if dep == name {
			return fmt.Errorf("Plugin %q cannot depend on itself", name)
		}
		if seenDeps[dep] {
			return fmt.Errorf("Plugin %q dependency %q is listed more than once", name, dep)
		}
		seenDeps[dep] = true
	}

	switch runtimeType {
	case PluginRuntimeMCP:
		if plugin.Tools == nil {
			return fmt.Errorf("Plugin %q requires explicit plugin.tools", name)
		}
		inbox := strings.TrimSpace(plugin.Inbox)
		if inbox == "" {
			return fmt.Errorf("Plugin %q requires plugin.inbox", name)
		}
		if strings.Contains(inbox, "*") {
			return fmt.Errorf("Plugin %q inbox must be an explicit tool name; wildcard is not allowed", name)
		}
		seen := make(map[string]bool, len(plugin.Tools))
		for _, raw := range plugin.Tools {
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
		if len(plugin.Depends) != 0 || len(plugin.Mounts) != 0 || len(plugin.Subscriptions) != 0 || len(plugin.Contributes) != 0 {
			return fmt.Errorf("MCP Plugin %q cannot declare Controller depends/mounts/subscriptions/contributions in V1", name)
		}
	case PluginRuntimeController:
		if len(plugin.Tools) != 0 || strings.TrimSpace(plugin.Inbox) != "" {
			return fmt.Errorf("Controller Plugin %q uses MCPX-hosted inbox and cannot declare MCP tools/inbox", name)
		}
		if len(plugin.Accepts) != 0 {
			return fmt.Errorf("Controller Plugin %q cannot accept Skill contributions in V1", name)
		}
		for alias, mount := range plugin.Mounts {
			if strings.TrimSpace(alias) == "" || strings.TrimSpace(mount.Plugin) == "" || strings.TrimSpace(mount.Tool) == "" {
				return fmt.Errorf("Controller Plugin %q mount %q requires plugin and tool", name, alias)
			}
			if mount.Plugin == name {
				return fmt.Errorf("Controller Plugin %q mount %q cannot target itself", name, alias)
			}
			if len(mount.Guards) > 0 && !mount.Automatic {
				return fmt.Errorf("Controller Plugin %q mount %q guards require automatic=true", name, alias)
			}
			for argument, guard := range mount.Guards {
				if strings.TrimSpace(argument) == "" {
					return fmt.Errorf("Controller Plugin %q mount %q guard argument cannot be empty", name, alias)
				}
				rules := 0
				if guard.Equals != "" {
					rules++
				}
				if guard.Prefix != "" {
					rules++
				}
				if len(guard.OneOf) > 0 {
					rules++
					seen := map[string]bool{}
					for _, value := range guard.OneOf {
						if strings.TrimSpace(value) == "" || seen[value] {
							return fmt.Errorf("Controller Plugin %q mount %q guard %q one_of values must be non-empty and unique", name, alias, argument)
						}
						seen[value] = true
					}
				}
				if rules != 1 {
					return fmt.Errorf("Controller Plugin %q mount %q guard %q must declare exactly one of equals/prefix/one_of", name, alias, argument)
				}
			}
		}
		for _, sub := range plugin.Subscriptions {
			if strings.TrimSpace(sub.Plugin) == "" || strings.TrimSpace(sub.Kind) != PluginSubscriptionInbox {
				return fmt.Errorf("Controller Plugin %q subscriptions must target plugin inbox", name)
			}
			if sub.Plugin == name {
				return fmt.Errorf("Controller Plugin %q cannot subscribe to itself", name)
			}
			scope := strings.TrimSpace(sub.Scope)
			if scope == "" {
				scope = PluginSubscriptionScopeWorkspace
			}
			if scope != PluginSubscriptionScopeWorkspace && scope != PluginSubscriptionScopeSessions {
				return fmt.Errorf("Controller Plugin %q subscription scope must be %q or %q", name, PluginSubscriptionScopeWorkspace, PluginSubscriptionScopeSessions)
			}
		}
		for _, contribution := range plugin.Contributes {
			if strings.TrimSpace(contribution.Plugin) == "" || strings.TrimSpace(contribution.Slot) == "" || strings.TrimSpace(contribution.Path) == "" {
				return fmt.Errorf("Controller Plugin %q contribution requires plugin, slot and path", name)
			}
			if contribution.Plugin == name {
				return fmt.Errorf("Controller Plugin %q cannot contribute to itself", name)
			}
			if !filepath.IsAbs(contribution.Path) {
				return fmt.Errorf("Controller Plugin %q contribution path must be absolute: %q", name, contribution.Path)
			}
		}
	}

	seenSlots := map[string]bool{}
	for _, slot := range plugin.Accepts {
		slotName := strings.TrimSpace(slot.Slot)
		if slotName == "" {
			return fmt.Errorf("Plugin %q contribution slot cannot be empty", name)
		}
		if seenSlots[slotName] {
			return fmt.Errorf("Plugin %q contribution slot %q is listed more than once", name, slot.Slot)
		}
		seenSlots[slotName] = true
		if slot.MaxBytes < 0 || slot.EffectiveMaxBytes() > PluginContributionHardMaxBytes {
			return fmt.Errorf("Plugin %q contribution slot %q max_bytes must be between 1 and %d when set", name, slot.Slot, PluginContributionHardMaxBytes)
		}
	}
	return nil
}

func validatePluginGraph(file MCPFile) error {
	plugins := map[string]MCPServer{}
	for name, server := range file.MCPServers {
		if server.IsPlugin {
			plugins[name] = server
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
				return fmt.Errorf("Plugin %q dependency %q is not a registered Plugin", name, dep)
			}
		}
		for alias, mount := range plugin.Mounts {
			if !declaredDeps[strings.TrimSpace(mount.Plugin)] {
				return fmt.Errorf("Controller Plugin %q mount %q target %q must be declared in depends", name, alias, mount.Plugin)
			}
			target, ok := plugins[mount.Plugin]
			if !ok {
				return fmt.Errorf("Controller Plugin %q mount %q targets unknown Plugin %q", name, alias, mount.Plugin)
			}
			if target.Plugin.RuntimeType() != PluginRuntimeMCP {
				return fmt.Errorf("Controller Plugin %q mount %q must target an MCP Plugin", name, alias)
			}
			found := false
			for _, tool := range target.Plugin.Tools {
				if strings.TrimSpace(tool) == strings.TrimSpace(mount.Tool) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("Controller Plugin %q mount %q targets unmounted tool %q on Plugin %q", name, alias, mount.Tool, mount.Plugin)
			}
		}
		for _, sub := range plugin.Subscriptions {
			if !declaredDeps[strings.TrimSpace(sub.Plugin)] {
				return fmt.Errorf("Controller Plugin %q subscription target %q must be declared in depends", name, sub.Plugin)
			}
			target, ok := plugins[sub.Plugin]
			if !ok {
				return fmt.Errorf("Controller Plugin %q subscription targets unknown Plugin %q", name, sub.Plugin)
			}
			if target.Plugin.RuntimeType() != PluginRuntimeMCP || strings.TrimSpace(target.Plugin.Inbox) == "" {
				return fmt.Errorf("Controller Plugin %q subscription target %q has no MCP inbox", name, sub.Plugin)
			}
		}
		for _, contribution := range plugin.Contributes {
			if !declaredDeps[strings.TrimSpace(contribution.Plugin)] {
				return fmt.Errorf("Controller Plugin %q contribution target %q must be declared in depends", name, contribution.Plugin)
			}
			target, ok := plugins[contribution.Plugin]
			if !ok {
				return fmt.Errorf("Controller Plugin %q contribution targets unknown Plugin %q", name, contribution.Plugin)
			}
			if target.Plugin.RuntimeType() != PluginRuntimeMCP || target.Plugin.RuntimeScope() != PluginScopeWorkspace {
				return fmt.Errorf("Controller Plugin %q contribution target %q must be a workspace-scoped MCP Plugin", name, contribution.Plugin)
			}
			accepted := false
			for _, slot := range target.Plugin.Accepts {
				if strings.TrimSpace(slot.Slot) == strings.TrimSpace(contribution.Slot) {
					accepted = true
					break
				}
			}
			if !accepted {
				return fmt.Errorf("Controller Plugin %q contribution slot %q is not accepted by Plugin %q", name, contribution.Slot, contribution.Plugin)
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
