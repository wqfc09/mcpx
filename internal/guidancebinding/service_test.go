package guidancebinding

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testService(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE remote_sessions (id TEXT PRIMARY KEY);
        CREATE TABLE guidance_bindings (
          remote_session_id TEXT NOT NULL,
          consumer_id TEXT NOT NULL,
          guidance_id TEXT NOT NULL,
          provider_plugin TEXT NOT NULL,
          scope TEXT NOT NULL,
          revision TEXT NOT NULL,
          content TEXT NOT NULL,
          delivery_key TEXT NOT NULL DEFAULT '',
          bound_at INTEGER NOT NULL,
          PRIMARY KEY (remote_session_id, consumer_id, guidance_id),
          FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
        );
        INSERT INTO remote_sessions(id) VALUES ('rs_1');`); err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	service.now = func() time.Time { return time.Date(2026, 8, 18, 6, 0, 0, 123000000, time.UTC) }
	return service
}

func TestBindPinsRevisionAndOnlyReplaysExactDelivery(t *testing.T) {
	service := testService(t)
	ctx := context.Background()
	abc := Candidate{GuidanceID: "comet.native.build", ProviderPlugin: "CometCoordinator", Scope: "context", Revision: "sha256:abc", Content: "build ABC"}
	first, err := service.Bind(ctx, "rs_1", "/agents/builder", "spawn-1", abc)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Delivered || first.Content != "build ABC" || first.Binding.Revision != "sha256:abc" {
		t.Fatalf("first delivery=%+v", first)
	}

	def := Candidate{GuidanceID: abc.GuidanceID, ProviderPlugin: abc.ProviderPlugin, Scope: abc.Scope, Revision: "sha256:def", Content: "build DEF"}
	retry, err := service.Bind(ctx, "rs_1", "/agents/builder", "spawn-1", def)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Delivered || retry.Content != "build ABC" || retry.Binding.Revision != "sha256:abc" {
		t.Fatalf("exact retry must replay pinned ABC: %+v", retry)
	}

	later, err := service.Bind(ctx, "rs_1", "/agents/builder", "spawn-2", def)
	if err != nil {
		t.Fatal(err)
	}
	if later.Delivered || later.Content != "" || later.Binding.Revision != "sha256:abc" {
		t.Fatalf("later bind must stay pinned without body replay: %+v", later)
	}

	newConsumer, err := service.Bind(ctx, "rs_1", "/agents/builder-2", "spawn-3", def)
	if err != nil {
		t.Fatal(err)
	}
	if !newConsumer.Delivered || newConsumer.Content != "build DEF" || newConsumer.Binding.Revision != "sha256:def" {
		t.Fatalf("new consumer must bind current DEF: %+v", newConsumer)
	}
}

func TestListConsumerReturnsMetadataWithoutBodies(t *testing.T) {
	service := testService(t)
	ctx := context.Background()
	for _, candidate := range []Candidate{
		{GuidanceID: "a", ProviderPlugin: "P", Scope: "attachment", Revision: "sha256:a", Content: "A"},
		{GuidanceID: "b", ProviderPlugin: "P", Scope: "attachment", Revision: "sha256:b", Content: "B"},
	} {
		if _, err := service.Bind(ctx, "rs_1", "att_1", "open-1", candidate); err != nil {
			t.Fatal(err)
		}
	}
	bindings, err := service.ListConsumer(ctx, "rs_1", "att_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[0].GuidanceID != "a" || bindings[1].GuidanceID != "b" {
		t.Fatalf("bindings=%+v", bindings)
	}
}
