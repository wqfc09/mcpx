package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

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
