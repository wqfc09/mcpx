package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationsRepairMissingAgentActivityTable(t *testing.T) {
	original := migrations
	activityIndex := -1
	for index, migration := range original {
		if strings.Contains(migration, "CREATE TABLE IF NOT EXISTS agent_activity_turns") {
			activityIndex = index
			break
		}
	}
	if activityIndex < 0 || activityIndex >= len(original)-1 {
		t.Fatalf("activity migration index=%d migrations=%d", activityIndex, len(original))
	}

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	migrations = original[:activityIndex]
	t.Cleanup(func() { migrations = original })
	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0)`, activityIndex+1); err != nil {
		t.Fatal(err)
	}

	migrations = original
	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'agent_activity_turns'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != "agent_activity_turns" {
		t.Fatalf("agent activity table=%q", table)
	}
}

func TestLatestMigrationAddsTerminalTaskLimitReason(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	var (
		name       string
		typeName   string
		notNull    int
		defaultVal sql.NullString
	)
	if err := db.QueryRow(`SELECT name, type, "notnull", dflt_value FROM pragma_table_info('terminal_tasks') WHERE name = 'limit_reason'`).Scan(&name, &typeName, &notNull, &defaultVal); err != nil {
		t.Fatal(err)
	}
	if name != "limit_reason" || typeName != "TEXT" || notNull != 1 || !defaultVal.Valid || defaultVal.String != "''" {
		t.Fatalf("limit_reason column = name=%q type=%q not_null=%d default=%v", name, typeName, notNull, defaultVal)
	}
}
