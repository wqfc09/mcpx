package server

import (
	"fmt"
	"sort"
	"strings"

	"mcpx/internal/config"
	"mcpx/internal/skill"
	"mcpx/internal/workspace"
)

type skillContributionOverlay struct {
	Content  string
	Revision string
	Items    []resolvedPluginContribution
}

func (r *Runtime) resolveSkillContributionOverlay(ws workspace.Workspace, skillName string) (skillContributionOverlay, error) {
	skillName = strings.TrimSpace(skillName)
	if skillName == "" || strings.TrimSpace(ws.Path) == "" {
		return skillContributionOverlay{}, nil
	}
	items := []resolvedPluginContribution{}
	for _, targetName := range r.sortedPluginNames() {
		mount := r.plugins[targetName]
		if mount.Server.Plugin == nil || mount.Server.Plugin.RuntimeType() != config.PluginRuntimeMCP {
			continue
		}
		slots := map[string]bool{}
		for _, accepted := range mount.Server.Plugin.Accepts {
			if strings.TrimSpace(accepted.Skill) == skillName {
				slots[strings.TrimSpace(accepted.Slot)] = true
			}
		}
		if len(slots) == 0 {
			continue
		}
		server, active, err := r.effectivePluginForWorkspace(ws.Path, targetName)
		if err != nil {
			return skillContributionOverlay{}, err
		}
		if !active {
			continue
		}
		resolved, _, err := r.resolvePluginContributions(ws, targetName, server)
		if err != nil {
			return skillContributionOverlay{}, err
		}
		for _, item := range resolved {
			if slots[item.Slot] {
				items = append(items, item)
			}
		}
	}
	if len(items) == 0 {
		return skillContributionOverlay{}, nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Target != items[j].Target {
			return items[i].Target < items[j].Target
		}
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		if items[i].Slot != items[j].Slot {
			return items[i].Slot < items[j].Slot
		}
		return items[i].Revision < items[j].Revision
	})
	revision := hashRevision(items)
	var body strings.Builder
	body.WriteString("\n\n<!-- mcpx-plugin-guidance revision=")
	body.WriteString(revision)
	body.WriteString(" -->\n## MCPX Plugin Guidance\n\n")
	body.WriteString("These short, revision-pinned coordination rules extend the original Skill; they do not replace its workflow or completion criteria.\n")
	for _, item := range items {
		body.WriteString("\n### ")
		body.WriteString(item.Source)
		body.WriteString(" · ")
		body.WriteString(item.Slot)
		body.WriteString(" · ")
		body.WriteString(item.Revision)
		body.WriteString("\n")
		body.WriteString(strings.TrimSpace(item.Content))
		body.WriteString("\n")
	}
	return skillContributionOverlay{Content: body.String(), Revision: revision, Items: items}, nil
}

func effectiveSkillDefinitionRevision(sk skill.Skill, overlay skillContributionOverlay) string {
	base := skillDefinitionRevision(sk)
	if overlay.Revision == "" {
		return base
	}
	return skillRevision([]string{base, overlay.Revision})
}

func appendSkillContribution(content string, overlay skillContributionOverlay) string {
	if overlay.Content == "" {
		return content
	}
	return strings.TrimRight(content, "\n") + overlay.Content
}

func skillContributionMetadata(overlay skillContributionOverlay) []map[string]any {
	result := make([]map[string]any, 0, len(overlay.Items))
	for _, item := range overlay.Items {
		result = append(result, map[string]any{
			"source": item.Source, "target": item.Target, "slot": item.Slot, "revision": item.Revision,
		})
	}
	return result
}

func skillContributionError(skillName string, err error) error {
	return fmt.Errorf("resolve contributions for Skill %q: %w", skillName, err)
}
