package guidance

import "testing"

func TestLoadAgent(t *testing.T) {
	cfg, err := LoadAgent()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "2.1" || len(cfg.Rules) < 8 || len(cfg.Rules) > 16 {
		t.Fatalf("unexpected compact guidance: %+v", cfg)
	}
	if len(cfg.ToolRouting["modify_files"]) == 0 || len(cfg.ToolRouting["inspect_environment"]) == 0 {
		t.Fatal("missing canonical routing")
	}
}
