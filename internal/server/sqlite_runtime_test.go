package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestEphemeralSQLiteRuntimeValidatesModeAndRedactsObservation(t *testing.T) {
	spec, err := ephemeralRuntimeSpecFromPayload(map[string]any{"runtime": "sqlite", "database": "data/app.db", "script": "SELECT 1"})
	if err != nil || spec == nil || spec.Database != "data/app.db" || spec.Executable != "" || spec.ScriptSHA256 == "" {
		t.Fatalf("sqlite runtime spec=%+v err=%v", spec, err)
	}
	if _, err := ephemeralRuntimeSpecFromPayload(map[string]any{"runtime": "sqlite", "script": "SELECT 1"}); err == nil {
		t.Fatal("sqlite runtime accepted missing database")
	}
	if _, err := ephemeralRuntimeSpecFromPayload(map[string]any{"runtime": "python", "database": "data.db", "script": "print(1)"}); err == nil {
		t.Fatal("python runtime accepted sqlite database option")
	}

	args := map[string]any{"action": "run", "runtime": "sqlite", "database": "data/app.db", "script": "SELECT 'secret-ish'", "purpose": "probe"}
	observed := observationArguments("execute", args)
	if observed["script"] != "[redacted ephemeral script]" || observed["script_sha256"] == "" || observed["script_bytes"] == nil || observed["database"] != "data/app.db" {
		t.Fatalf("observed runtime args=%+v", observed)
	}
	if args["script"] != "SELECT 'secret-ish'" {
		t.Fatal("observation redaction mutated the live request")
	}
}

func TestEphemeralSQLiteRuntimeQueriesWorkspaceDatabaseReadonly(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	databasePath := filepath.Join(workspace.Path, "sample.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE items(id INTEGER PRIMARY KEY, name TEXT); INSERT INTO items(name) VALUES ('alpha'), ('beta')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	result := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "inspect fixture rows",
		"runtime": "sqlite", "database": "sample.db", "script": "SELECT id, name FROM items ORDER BY id",
	})
	if !statusOK(result) {
		t.Fatalf("sqlite runtime result=%+v", result)
	}
	data := result["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	if data["runtime"] != "sqlite" || data["database"] != "sample.db" || data["readonly"] != true || data["row_count"] != float64(2) || len(rows) != 2 {
		t.Fatalf("sqlite runtime metadata=%+v", data)
	}
	if _, exists := data["execution_task_id"]; exists {
		t.Fatalf("in-process sqlite query must not create a fake terminal task: %+v", data)
	}

	mutation := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "attempt a forbidden mutation",
		"runtime": "sqlite", "database": "sample.db", "script": "DELETE FROM items",
	})
	if statusOK(mutation) {
		t.Fatalf("sqlite runtime accepted mutation=%+v", mutation)
	}

	rt.cfg.Security.Files.Deny = append(rt.cfg.Security.Files.Deny, `^sample\.db$`)
	denied := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "query a file denied by policy",
		"runtime": "sqlite", "database": "nested/../sample.db", "script": "SELECT 1",
	})
	if statusOK(denied) || errorCode(denied) != "file_denied" {
		t.Fatalf("sqlite file policy must see normalized path aliases: %+v", denied)
	}

	absolute := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "reject an absolute database path",
		"runtime": "sqlite", "database": databasePath, "script": "SELECT 1",
	})
	if statusOK(absolute) || errorCode(absolute) != "sqlite_query_error" {
		t.Fatalf("sqlite runtime accepted absolute database path=%+v", absolute)
	}
}
