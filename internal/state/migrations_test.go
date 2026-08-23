package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasedMigrationPrefixIsStable(t *testing.T) {
	if len(migrations) < 29 {
		t.Fatalf("migration count=%d want>=29", len(migrations))
	}
	var history strings.Builder
	for _, migration := range migrations[:28] {
		history.WriteString(migration)
		history.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(history.String()))
	got := hex.EncodeToString(digest[:])
	const want = "63a8e449e665d355c492a6cd0afd97d91447197e7fc3daee162aa8bbf9536c7e"
	if got != want {
		t.Fatalf("released migration history changed: got=%s want=%s; append a new migration instead", got, want)
	}
}

func TestMigrationsRepairMissingAgentActivityTable(t *testing.T) {
	if len(migrations) < 29 {
		t.Fatalf("migration count=%d want>=29", len(migrations))
	}
	if !strings.Contains(migrations[28], "CREATE TABLE IF NOT EXISTS agent_activity_turns") {
		t.Fatal("migration 29 must repair agent_activity_turns")
	}

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE observation_events (
		sequence INTEGER PRIMARY KEY AUTOINCREMENT,
		remote_session_id TEXT NOT NULL DEFAULT '',
		event_type TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE remote_sessions (
		id TEXT PRIMARY KEY,
		last_active_at INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	// Reproduce the live poisoned database: every historical version through 28
	// is recorded as applied, while the Activity state table and the four Activity
	// V2 observation columns are physically absent.
	for version := 1; version <= 28; version++ {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0)`, version); err != nil {
			t.Fatal(err)
		}
	}

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
	var repairedColumns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('observation_events')
		WHERE name IN ('turn_id','activity_sequence','activity_kind','related_call_id')`).Scan(&repairedColumns); err != nil {
		t.Fatal(err)
	}
	if repairedColumns != 4 {
		t.Fatalf("activity observation columns=%d want=4", repairedColumns)
	}
	var activityIndex string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_observation_events_activity'`).Scan(&activityIndex); err != nil {
		t.Fatal(err)
	}
	if activityIndex != "idx_observation_events_activity" {
		t.Fatalf("activity observation index=%q", activityIndex)
	}
	var latest int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if latest != 31 {
		t.Fatalf("latest migration=%d want=31", latest)
	}
}

func TestLatestMigrationAddsRemoteSessionWorkspaceID(t *testing.T) {
	if len(migrations) < 31 {
		t.Fatalf("migration count=%d want>=31", len(migrations))
	}
	if !strings.Contains(migrations[30], "ALTER TABLE remote_sessions ADD COLUMN workspace_id") {
		t.Fatal("migration 31 must add remote_sessions.workspace_id")
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM pragma_table_info('remote_sessions') WHERE name = 'workspace_id'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "workspace_id" {
		t.Fatalf("workspace id column=%q", name)
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
