package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateMCPFileAcceptsNativeDependencyGraph(t *testing.T) {
	guidance := filepath.Join(t.TempDir(), "creator-review.md")
	file := MCPFile{MCPServers: map[string]MCPServer{
		"Comet": {
			Command: "node", IsPlugin: true,
			Plugin: &MCPPlugin{
				Scope: PluginScopeWorkspace, Tools: []string{"comet_context", "comet_creator"}, Inbox: "comet_inbox",
				Accepts: []MCPPluginContributionSlot{{Slot: "creator.reviewer.guidance", MaxBytes: 1024}},
				TUI:     &MCPPluginTUI{Title: "Comet", Command: "comet", Args: []string{"tui"}},
			},
		},
		"JEA": {
			Command: "node", IsPlugin: true,
			Plugin: &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"agent_spawn", "agent_status"}, Inbox: "agent_inbox"},
		},
		"CreatorCoordinator": {
			Command: "node", IsPlugin: true,
			Plugin: &MCPPlugin{
				Runtime: PluginRuntimeNative,
				Scope:   PluginScopeWorkspace,
				Depends: []string{"Comet", "JEA"},
				Mounts: map[string]MCPPluginMount{
					"context": {Plugin: "Comet", Tool: "comet_context", Automatic: true},
					"worker":  {Plugin: "JEA", Tool: "agent_spawn", Automatic: true},
				},
				Subscriptions: []MCPPluginSubscription{{Plugin: "Comet", Kind: PluginSubscriptionInbox}, {Plugin: "JEA", Kind: PluginSubscriptionInbox}},
				Contributes:   []MCPPluginContribution{{Plugin: "Comet", Slot: "creator.reviewer.guidance", Path: guidance}},
				TUI:           &MCPPluginTUI{Title: "Coordinator", Command: "node", Args: []string{"coordinator-tui.js"}},
			},
		},
	}}
	if err := ValidateMCPFile(file); err != nil {
		t.Fatalf("valid Native Plugin graph rejected: %v", err)
	}
}

func TestValidateMCPFileRejectsNativeGraphViolations(t *testing.T) {
	baseTarget := MCPServer{
		Command: "node", IsPlugin: true,
		Plugin: &MCPPlugin{
			Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox",
			Accepts: []MCPPluginContributionSlot{{Slot: "build.guidance", MaxBytes: 512}},
		},
	}
	native := func() MCPServer {
		return MCPServer{Command: "node", IsPlugin: true, Plugin: &MCPPlugin{Runtime: PluginRuntimeNative, Scope: PluginScopeWorkspace}}
	}

	t.Run("tui requires command", func(t *testing.T) {
		got := baseTarget
		got.Plugin = &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox", TUI: &MCPPluginTUI{Title: "broken"}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": got}})
		if err == nil || !strings.Contains(err.Error(), "tui requires command") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("unknown dependency", func(t *testing.T) {
		got := native()
		got.Plugin.Depends = []string{"missing"}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"native": got}})
		if err == nil || !strings.Contains(err.Error(), "requires unknown") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("mount must be public target tool", func(t *testing.T) {
		got := native()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Mounts = map[string]MCPPluginMount{"bad": {Plugin: "target", Tool: "missing"}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "native": got}})
		if err == nil || !strings.Contains(err.Error(), "unavailable Tool") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("native cycle", func(t *testing.T) {
		a, b := native(), native()
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
		got := native()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Contributes = []MCPPluginContribution{{Plugin: "target", Slot: "build.guidance", Path: filepath.Join(t.TempDir(), "g.md")}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": target, "native": got}})
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

	t.Run("mcp runtime may declare requires", func(t *testing.T) {
		target := baseTarget
		target.Plugin = &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox", Depends: []string{"other"}}
		other := baseTarget
		if err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": target, "other": other}}); err != nil {
			t.Fatalf("requires should be runtime-independent: %v", err)
		}
	})

	t.Run("mount target must be declared dependency", func(t *testing.T) {
		got := native()
		got.Plugin.Mounts = map[string]MCPPluginMount{"context": {Plugin: "target", Tool: "context", Automatic: true}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "native": got}})
		if err == nil || !strings.Contains(err.Error(), "requires.plugins") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("subscription scope must be workspace or sessions", func(t *testing.T) {
		got := native()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Subscriptions = []MCPPluginSubscription{{Plugin: "target", Kind: PluginSubscriptionInbox, Scope: "global"}}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "native": got}})
		if err == nil || !strings.Contains(err.Error(), "watch scope") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("guards require automatic and exactly one rule", func(t *testing.T) {
		got := native()
		got.Plugin.Depends = []string{"target"}
		got.Plugin.Mounts = map[string]MCPPluginMount{
			"context": {Plugin: "target", Tool: "context", Guards: map[string]MCPPluginStringGuard{"action": {Equals: "read"}}},
		}
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "native": got}})
		if err == nil || !strings.Contains(err.Error(), "constraints require automatic=true") {
			t.Fatalf("err=%v", err)
		}

		got.Plugin.Mounts["context"] = MCPPluginMount{
			Plugin: "target", Tool: "context", Automatic: true,
			Guards: map[string]MCPPluginStringGuard{"action": {Equals: "read", Prefix: "r"}},
		}
		err = ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"target": baseTarget, "native": got}})
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestValidateMCPFileAcceptsOptionalPluginGuidanceAndRejectsAmbiguity(t *testing.T) {
	root := t.TempDir()
	attachmentPath := filepath.Join(root, "coordination.md")
	contextPath := filepath.Join(root, "builder.md")
	base := MCPServer{
		Command: "node", IsPlugin: true,
		Plugin: &MCPPlugin{
			Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox",
			Guidance: []MCPPluginGuidance{
				{ID: "demo.coordination", Scope: PluginGuidanceScopeAttachment, Path: attachmentPath},
				{ID: "demo.build", Scope: PluginGuidanceScopeContext, Path: contextPath},
			},
		},
	}
	if err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"demo": base}}); err != nil {
		t.Fatalf("valid optional Guidance rejected: %v", err)
	}

	t.Run("invalid scope", func(t *testing.T) {
		got := base
		plugin := *base.Plugin
		plugin.Guidance = []MCPPluginGuidance{{ID: "demo.bad", Scope: "runtime", Path: contextPath}}
		got.Plugin = &plugin
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"demo": got}})
		if err == nil || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		got := base
		plugin := *base.Plugin
		plugin.Guidance = []MCPPluginGuidance{{ID: "demo.bad", Scope: PluginGuidanceScopeContext, Path: "./builder.md"}}
		got.Plugin = &plugin
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"demo": got}})
		if err == nil || !strings.Contains(err.Error(), "path must be absolute") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("duplicate id across providers", func(t *testing.T) {
		other := base
		otherPlugin := *base.Plugin
		otherPlugin.Guidance = []MCPPluginGuidance{{ID: "demo.coordination", Scope: PluginGuidanceScopeAttachment, Path: filepath.Join(root, "other.md")}}
		other.Plugin = &otherPlugin
		err := ValidateMCPFile(MCPFile{MCPServers: map[string]MCPServer{"demo": base, "other": other}})
		if err == nil || !strings.Contains(err.Error(), "declared by both") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestPluginRevisionsSeparateTUIAndGuidanceFromBusinessRuntime(t *testing.T) {
	base := MCPServer{
		Type: "stdio", Command: "node", Args: []string{"plugin.js"}, Env: map[string]string{"A": "1"},
		IsPlugin: true, Trust: true, Description: "one",
		Plugin: &MCPPlugin{Scope: PluginScopeWorkspace, Tools: []string{"context"}, Inbox: "inbox", TUI: &MCPPluginTUI{Title: "One", Command: "node", Args: []string{"monitor.js"}}},
	}
	definition := PluginDefinitionRevision(base)
	runtime := PluginRuntimeRevision(base)

	uiOnly := base
	uiPlugin := *base.Plugin
	uiPlugin.TUI = &MCPPluginTUI{Title: "Two", Command: "node", Args: []string{"monitor-v2.js"}}
	uiOnly.Plugin = &uiPlugin
	uiOnly.Description = "two"
	if PluginDefinitionRevision(uiOnly) == definition {
		t.Fatal("TUI/description change must change full definition revision")
	}
	if PluginRuntimeRevision(uiOnly) != runtime {
		t.Fatal("TUI/description change must not change business runtime revision")
	}

	runtimeChange := base
	runtimeChange.Env = map[string]string{"A": "2"}
	if PluginRuntimeRevision(runtimeChange) == runtime {
		t.Fatal("runtime environment change must change business runtime revision")
	}
	if MCPRegistrationFingerprint(uiOnly) == MCPRegistrationFingerprint(base) {
		t.Fatal("TUI executable contribution must remain part of the existing registration fingerprint")
	}

	guidanceOnly := base
	guidancePlugin := *base.Plugin
	guidancePlugin.Guidance = []MCPPluginGuidance{{ID: "demo.build", Scope: PluginGuidanceScopeContext, Path: filepath.Join(t.TempDir(), "builder.md")}}
	guidanceOnly.Plugin = &guidancePlugin
	if PluginDefinitionRevision(guidanceOnly) == definition {
		t.Fatal("Guidance declaration must change full definition revision")
	}
	if MCPRegistrationFingerprint(guidanceOnly) == MCPRegistrationFingerprint(base) {
		t.Fatal("Guidance declaration must remain part of the trusted registration fingerprint")
	}
	if PluginRuntimeRevision(guidanceOnly) != runtime {
		t.Fatal("Guidance declaration must not change business runtime revision")
	}
}

func TestMCPRegistrationFingerprintIncludesControllerContract(t *testing.T) {
	base := MCPServer{
		Command: "node", IsPlugin: true,
		Plugin: &MCPPlugin{
			Runtime: PluginRuntimeNative, Scope: PluginScopeWorkspace,
			Depends: []string{"Comet"},
			Mounts:  map[string]MCPPluginMount{"context": {Plugin: "Comet", Tool: "comet_context", Automatic: true}},
		},
	}
	fingerprint := MCPRegistrationFingerprint(base)
	variant := base
	variant.Plugin = &MCPPlugin{
		Runtime: PluginRuntimeNative, Scope: PluginScopeWorkspace,
		Depends: []string{"Comet"},
		Mounts:  map[string]MCPPluginMount{"context": {Plugin: "Comet", Tool: "comet_context", Automatic: false}},
	}
	if got := MCPRegistrationFingerprint(variant); got == fingerprint {
		t.Fatal("automatic mount policy must invalidate Plugin registration fingerprint")
	}
}
