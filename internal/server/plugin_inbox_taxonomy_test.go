package server

import "testing"

func TestPluginInboxTaxonomyIsForwardedWithProviderIdentityButDoesNotWake(t *testing.T) {
	item := pluginInboxItem{
		Plugin: "JEA",
		Status: "succeeded",
		Result: map[string]any{
			"timed_out": true,
			"items":     []any{},
			"taxonomy": map[string]any{
				"kind":    "agent_waiting",
				"wait_ms": float64(35000),
				"summary": "wait",
			},
		},
	}
	got := pluginInboxTaxonomies(item)
	if len(got) != 1 || got[0]["plugin"] != "JEA" || got[0]["kind"] != "agent_waiting" || got[0]["summary"] != "wait" {
		t.Fatalf("taxonomies=%+v", got)
	}
	if pluginInboxItemWakes(item) {
		t.Fatal("taxonomy-only quiet Inbox must not become an immediate wake event")
	}
}
