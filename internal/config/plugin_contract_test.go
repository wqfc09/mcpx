package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateMCPFileAcceptsControllerDependencyGraph(t *testing.T) {
	guidance := filepath.Join(t.TempDir(), "creator-review.md")
	file := MCPFile{MCPServers: map[string]MCPServer{
		"Comet": {
			Command: "node", IsPlugin: true,
			Plugin: &MCPPlugin{
				Scope: PluginScopeWorkspace, Tools: []string{"comet_context", "comet_creator"}, Inbox: "comet_inbox",
				Accepts: []MCPPluginContributionSlot{{Slot: "creator.reviewer.guidance", MaxBytes: 1024}},
			},
		},
		"JEA": {
			Command: "node", IsPlugin: true,
			Plugin: &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"agent_spawn", "agent_status"}, Inbox: "agent_inbox"},
		},
		"CreatorCoordinator": {
			Command: "node", IsPlugin: true,
			Plugin: &MCPPlugin{
				Runtime: PluginRuntimeController,
				Scope:   PluginScopeWorkspace,
				Depends: []string{"Comet", "JEA"},
				Mounts: map[string]MCPPluginMount{
					"context": {Plugin: "Comet", Tool: "comet_context", Automatic: true},
					"worker":  {Plugin: "JEA", Tool: "agent_spawn", Automatic: true},
				},
				Subscriptions: []MCPPluginSubscription{{Plugin: "Comet", Kind: PluginSubscriptionInbox}, {Plugin: "JEA", Kind: PluginSubscriptionInbox}},
				Contributes:   []MCPPluginContribution{{Plugin: "Comet", Slot: "creator.reviewer.guidance", Path: guidance}},
			},
		},
	}}
	if err := ValidateMCPFile(file); err != nil {
		t.Fatalf("valid Controller Plugin graph rejected: %v", err)
	}
}

func TestValidateMCPFileRejectsControllerGraphViolations(t *testing.T) {
	baseTarget := MCPServer{
		Command: "node", IsPlugin: true,
		Plugin: &MCPPlugin{
			Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox",
			Accepts: []MCPPluginContributionSlot{{Slot: "build.guidance", MaxBytes: 512}},
		},
	}
	controller := func() MCPServer {
		return MCPServer{Command: "node", IsPlugin: true, Plugin: &MCPPlugin{Runtime: PluginRuntimeController, Scope: PluginScopeWorkspace}}
	}

	t.Run("unknown dependency", func(t *testing.T) {
		got := controller()
		got.Plugin.Depends = []string{"missing"}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"controller": got}})
		if err == nil || !strings.Contains(err.Error(), "dependency") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("mount must be public target tool", func(t *testing.T) {
		got := controller()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Mounts = map[string]MCPPluginMount{"bad": {Plugin: "target", Tool: "missing"}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "controller": got}})
		if err == nil || !strings.Contains(err.Error(), "unmounted tool") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("controller cycle", func(t *testing.T) {
		a, b := controller(), controller()
		a.Plugin.Depends = []string{"b"}
		b.Plugin.Depends = []string{"a"}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"a": a, "b": b}})
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("contribution requires workspace target", func(t *testing.T) {
		target := baseTarget
		target.Plugin = &MCPPlugin{Scope: PluginScopeInstance, Tools: []string{"context"}, Inbox: "inbox", Accepts: []MCPPluginContributionSlot{{Slot: "build.guidance"}}}
		got := controller()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Contributes = []MCPPluginContribution{{Plugin: "target", Slot: "build.guidance", Path: filepath.Join(t.TempDir(), "g.md")}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": target, "controller": got}})
		if err == nil || !strings.Contains(err.Error(), "workspace-scoped") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("slot hard limit", func(t *testing.T) {
		target := baseTarget
		target.Plugin = &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox", Accepts: []MCPPluginContributionSlot{{Slot: "build.guidance", MaxBytes: PluginContributionHardMaxBytes + 1}}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": target}})
		if err == nil || !strings.Contains(err.Error(), "max_bytes") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("mcp runtime cannot declare controller dependency", func(t *testing.T) {
		target := baseTarget
		target.Plugin = &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox", Depends: []string{"other"}}
		other := baseTarget
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": target, "other": other}})
		if err == nil || !strings.Contains(err.Error(), "cannot declare Controller depends") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("mount target must be declared dependency", func(t *testing.T) {
		got := controller()
		got.Plugin.Mounts = map[string]MCPPluginMount{"context": {Plugin: "target", Tool: "context", Automatic: true}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "controller": got}})
		if err == nil || !strings.Contains(err.Error(), "must be declared in depends") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("subscription scope must be workspace or sessions", func(t *testing.T) {
		got := controller()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Subscriptions = []MCPPluginSubscription{{Plugin: "target", Kind: PluginSubscriptionInbox, Scope: "global"}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "controller": got}})
		if err == nil || !strings.Contains(err.Error(), "subscription scope") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("guards require automatic and exactly one rule", func(t *testing.T) {
		got := controller()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Mounts = map[string]MCPPluginMount{
			"context": {Plugin: "target", Tool: "context", Guards: map[string]MCPPluginStringGuard{"action": {Equals: "read"}}},
		}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "controller": got}})
		if err == nil || !strings.Contains(err.Error(), "guards require automatic=true") {
			t.Fatalf("err=%v", err)
		}

		got.Plugin.Mounts["context"] = MCPPluginMount{
			Plugin: "target", Tool: "context", Automatic: true,
			Guards: map[string]MCPPluginStringGuard{"action": {Equals: "read", Prefix: "r"}},
		}
		err = ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "controller": got}})
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestMCPRegistrationFingerprintIncludesControllerContract(t *testing.T) {
	base := MCPServer{
		Command: "node", IsPlugin: true,
		Plugin: &MCPPlugin{
			Runtime: PluginRuntimeController, Scope: PluginScopeWorkspace,
			Depends: []string{"Comet"},
			Mounts:  map[string]MCPPluginMount{"context": {Plugin: "Comet", Tool: "comet_context", Automatic: true}},
		},
	}
	fingerprint := MCPRegistrationFingerprint(base)
	variant := base
	variant.Plugin = &MCPPlugin{
		Runtime: PluginRuntimeController, Scope: PluginScopeWorkspace,
		Depends: []string{"Comet"},
		Mounts:  map[string]MCPPluginMount{"context": {Plugin: "Comet", Tool: "comet_context", Automatic: false}},
	}
	if got := MCPRegistrationFingerprint(variant); got == fingerprint {
		t.Fatal("automatic mount policy must invalidate Plugin registration fingerprint")
	}
}
