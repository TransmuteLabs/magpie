package usage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

func TestDevinSQLiteAndFolderUsage(t *testing.T) {
	pageHome(t)
	if LogCalls != nil {
		t.Fatal("fixture requires ordinary session discovery")
	}
	now := holdClock(t, time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC))
	root := t.TempDir()
	t.Setenv("DEVIN_SESSIONS_DIR", root)
	all := false
	if err := sessions.SetReads(&all, map[string]bool{"devin": true}); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(root, "cli")
	if err := os.MkdirAll(cli, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(cli, "sessions.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(sessions.Reset)
	exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, working_directory TEXT, last_activity_at INTEGER)`)
	exec(`CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY, session_id TEXT, chat_message TEXT)`)
	for _, id := range []string{"db", "shared"} {
		exec(`INSERT INTO sessions VALUES (?, ?, ?)`, id, "/fixture/"+id, now.Unix())
	}
	late, tied, old := now.Add(-time.Minute), now.Add(-2*time.Minute), now.Add(-3*time.Minute)
	const missingMetrics = `"invalid"`
	metrics := func(input int) string {
		return fmt.Sprintf(`{"input_tokens":%d,"output_tokens":3,"cache_read_tokens":5,"cache_creation_tokens":7,"ttft_ms":11,"total_time_ms":13}`, input)
	}
	msg := func(role, id string, at time.Time, counts string) string {
		return fmt.Sprintf(`{"role":%q,"content":"synthetic","metadata":{"request_id":%q,"generation_model":"swe-2-high","created_at":%q,"metrics":%s}}`, role, id, at.Format(time.RFC3339Nano), counts)
	}
	for index, body := range []string{
		msg("assistant", "db-late", late, metrics(17)),
		msg("assistant", "db-z-first", tied, missingMetrics),
		msg("assistant", "db-z-first", tied, metrics(19)),
		msg("assistant", "db-a-second", tied, metrics(23)),
		msg("assistant", "db-z-first", late, metrics(101)),
		msg("user", "db-user", late, metrics(103)),
	} {
		exec(`INSERT INTO message_nodes VALUES (?, ?, ?)`, index+1, "db", body)
	}
	exec(`INSERT INTO message_nodes VALUES (7, ?, ?)`, "shared", msg("assistant", "shared-db", old, metrics(29)))
	folder := func(id, req string, at, modified time.Time, input int) string {
		t.Helper()
		dir := filepath.Join(cli, "sessions", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"working_directory":"/fixture/`+id+`-file"}`), 0600); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "log.jsonl")
		body := `{"kind":"node","node":{"message":` + msg("assistant", req, at, metrics(input)) + `}}`
		if err := os.WriteFile(log, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(log, modified, modified); err != nil {
			t.Fatal(err)
		}
		return log
	}
	folderPath := folder("folder", "folder-only", now, now, 31)
	sharedPath := folder("shared", "shared-file", old, old, 37)

	type expected struct {
		session, cwd string
		at           time.Time
		in           int
	}
	want := map[string]expected{
		"devin:db-late":     {"db", "/fixture/db", late, 17},
		"devin:db-z-first":  {"db", "/fixture/db", tied, 19},
		"devin:db-a-second": {"db", "/fixture/db", tied, 23},
		"devin:folder-only": {"folder", "/fixture/folder-file", now, 31},
		"devin:shared-db":   {"shared", "/fixture/shared", old, 29},
	}
	page := QueryPage(All, Filter{Agent: "devin"}, 0, 100)
	if page.Total != 5 || len(page.Rows) != 5 || page.Direct.Calls != 5 || page.Through.Calls != 0 {
		t.Fatalf("unexpected Usage counts: total=%d rows=%d direct=%d through=%d", page.Total, len(page.Rows), page.Direct.Calls, page.Through.Calls)
	}
	usageRows := make(map[int]expected, len(want))
	for _, call := range want {
		usageRows[call.in] = call
	}
	for index, row := range page.Rows {
		expected, ok := usageRows[row.Input]
		if !ok || row.Session != expected.session || !row.Time.Equal(expected.at) ||
			row.Agent != "devin" || row.Source != "log" || row.Provider != UnknownProvider || row.Status != 0 ||
			row.Model != "swe-2" || row.Served != "swe-2" || row.Effort != "high" ||
			row.Output != 3 || row.CacheRead != 5 || row.CacheWrite != 7 ||
			row.TTFT != 11 || row.Millis != 13 || row.ProviderAccount != "" || row.SessionAccount != "" ||
			row.SessionProvider != "" || row.Upstream != "" || row.SessionOfficialLogin {
			t.Fatalf("unexpected native Usage row: source=%q record=%+v", row.Source, row.Record)
		}
		if index > 0 && row.Time.After(page.Rows[index-1].Time) {
			t.Fatal("Usage is out of newest-first order")
		}
		delete(usageRows, row.Input)
	}
	if len(usageRows) != 0 {
		t.Fatalf("missing native Usage rows: %+v", usageRows)
	}
	paths := map[string]string{"devin:db": dbPath + "#db", "devin:shared": dbPath + "#shared", "devin:folder": folderPath}
	for _, source := range sessions.CallSources() {
		path, ok := paths[source.Key]
		if !ok || source.Agent != "devin" || source.Path != path {
			t.Fatalf("unexpected native call source: %+v", source)
		}
		delete(paths, source.Key)
		var got []string
		for _, call := range sessions.ReadCallSource(source) {
			expected, ok := want[call.Msg]
			if !ok || source.Key != "devin:"+expected.session || call.Agent != "devin" || call.Session != expected.session ||
				!call.Time.Equal(expected.at) || call.Cwd != expected.cwd || call.Model != "swe-2" || call.Effort != "high" ||
				call.Tokens != (sessions.Tokens{Input: expected.in, Output: 3, CacheRead: 5, CacheWrite: 7}) ||
				call.TTFT != 11 || call.Millis != 13 || call.RequestID != "" || call.AccountID != "" || call.UserID != "" || call.Upstream != "" {
				t.Fatalf("unexpected call from %s: %+v", source.Key, call)
			}
			got = append(got, call.Msg)
		}
		order := []string{"devin:db-z-first", "devin:db-a-second", "devin:db-late"}
		switch source.Key {
		case "devin:shared":
			order = []string{"devin:shared-db"}
		case "devin:folder":
			order = []string{"devin:folder-only"}
		}
		if !slices.Equal(got, order) {
			t.Fatalf("source %s order %v, want %v", source.Key, got, order)
		}
	}
	if len(paths) != 0 {
		t.Fatalf("unlisted native sources %v", paths)
	}
	for _, modified := range []time.Time{now, now.Add(time.Second)} {
		if err := os.Chtimes(sharedPath, modified, modified); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, source := range sessions.CallSources() {
			if source.Key != "devin:shared" {
				continue
			}
			count++
			calls := sessions.ReadCallSource(source)
			if source.Path != sharedPath || len(calls) != 1 || calls[0].Msg != "devin:shared-file" ||
				calls[0].Cwd != "/fixture/shared-file" || calls[0].Input != 37 {
				t.Fatalf("folder must win at %s: %+v %+v", modified, source, calls)
			}
		}
		if count != 1 {
			t.Fatalf("shared source count at %s: %d", modified, count)
		}
	}
}
