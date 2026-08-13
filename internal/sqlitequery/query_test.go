package sqlitequery

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestQueryReadsWorkspaceDatabaseAndBoundsRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.db")
	createFixtureDatabase(t, path)

	result, err := Query(context.Background(), root, "sample.db", "WITH picked AS (SELECT id, name FROM items) SELECT id, name, 'semi;colon' AS note FROM picked ORDER BY id", 2, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result.Columns) != "[id name note]" || len(result.Rows) != 2 || !result.Truncated {
		t.Fatalf("query result=%+v", result)
	}
	if fmt.Sprint(result.Rows[0]) != "[1 alpha semi;colon]" || result.RowCount != 2 {
		t.Fatalf("query rows=%+v", result.Rows)
	}
}

func TestQueryRejectsStatementsMutationsAndPathEscape(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.db")
	createFixtureDatabase(t, path)

	for _, query := range []string{
		"SELECT 1; SELECT 2",
		"DELETE FROM items",
		"ATTACH DATABASE 'other.db' AS other",
		"PRAGMA writable_schema = ON",
	} {
		if _, err := Query(context.Background(), root, "sample.db", query, 10, 64<<10); err == nil {
			t.Fatalf("unsafe sqlite input was accepted: %q", query)
		}
	}
	if _, err := Query(context.Background(), root, "../outside.db", "SELECT 1", 10, 64<<10); err == nil {
		t.Fatal("workspace path escape was accepted")
	}
	if _, err := Query(context.Background(), root, "missing.db", "SELECT 1", 10, 64<<10); err == nil {
		t.Fatal("missing database was created or accepted")
	}
}

func TestReadonlyConnectionDisablesAttachAtSQLiteEngine(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main.db")
	otherPath := filepath.Join(root, "other.db")
	createFixtureDatabase(t, mainPath)
	createFixtureDatabase(t, otherPath)

	db, conn, err := openReadonlyConnection(context.Background(), root, "main.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "ATTACH DATABASE ? AS other", otherPath); err == nil {
		t.Fatal("SQLITE_LIMIT_ATTACHED=0 did not reject ATTACH")
	}
	if _, err := conn.ExecContext(context.Background(), "DELETE FROM items"); err == nil {
		t.Fatal("readonly SQLite connection accepted a mutation")
	}
}

func TestQueryRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.db")
	createFixtureDatabase(t, outside)
	link := filepath.Join(root, "outside.db")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Query(context.Background(), root, "outside.db", "SELECT 1", 10, 64<<10); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}

func createFixtureDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE items(id INTEGER PRIMARY KEY, name TEXT); INSERT INTO items(name) VALUES ('alpha'), ('beta'), ('gamma')`); err != nil {
		t.Fatal(err)
	}
}
