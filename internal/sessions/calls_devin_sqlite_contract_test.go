package sessions

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

const devinFixtureNodeSchema = `CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY, session_id TEXT, chat_message TEXT)`

func openDevinTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func execDevinTestSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func devinTestRow(id, content string) string {
	return fmt.Sprintf(`{"role":"assistant","content":%q,"metadata":{"request_id":%q,"generation_model":"swe-2-high","created_at":"2026-11-01T15:00:00Z","metrics":{"input_tokens":2,"output_tokens":3,"cache_read_tokens":5,"cache_creation_tokens":7,"ttft_ms":11,"total_time_ms":13}}}`, content, id)
}

func TestDevinSQLiteReadBoundaries(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	t.Run("acquire", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.db")
		source := file{path: path + "#test", agent: "devin"}
		result, readOK := readDevinCalls(source)
		if readOK || result.Session != "test" || len(result.Calls) != 0 {
			t.Fatalf("missing DB: readOK=%t result=%+v", readOK, result)
		}
	})
	t.Run("query", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "query.db")
		db := openDevinTestDB(t, path)
		t.Cleanup(Reset)
		execDevinTestSQL(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, working_directory TEXT)`)
		execDevinTestSQL(t, db, `INSERT INTO sessions VALUES ('test', '/fixture/sqlite')`)
		execDevinTestSQL(t, db, `CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY, session_id TEXT)`)
		result, readOK := readDevinCalls(file{path: path + "#test", agent: "devin"})
		if !readOK || result.Session != "test" || len(result.Calls) != 0 {
			t.Fatalf("missing projection: readOK=%t result=%+v", readOK, result)
		}
	})
	t.Run("iteration", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "iteration.db")
		db := openDevinTestDB(t, path)
		execDevinTestSQL(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, working_directory TEXT)`)
		execDevinTestSQL(t, db, `INSERT INTO sessions VALUES ('test', '/fixture/sqlite')`)
		execDevinTestSQL(t, db, devinFixtureNodeSchema)
		t.Cleanup(Reset)
		for index, body := range []string{devinTestRow("before", "synthetic"), "{", devinTestRow("after", "synthetic")} {
			execDevinTestSQL(t, db, `INSERT INTO message_nodes VALUES (?, 'test', ?)`, index+1, body)
		}
		result, readOK := readDevinCalls(file{path: path + "#test", agent: "devin"})
		var ids []string
		for _, call := range result.Calls {
			ids = append(ids, call.Msg)
			if call.Session != "test" || call.Cwd != "/fixture/sqlite" {
				t.Fatalf("unexpected SQL prefix call %+v", call)
			}
		}
		if !readOK || !slices.Equal(ids, []string{"devin:before"}) {
			t.Fatalf("malformed SQL JSON: readOK=%t rows=%v", readOK, ids)
		}
	})
}
