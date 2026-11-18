package sessions

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeHistoryDelegationGuardsAuxiliaryTitles(t *testing.T) {
	for _, mode := range []string{"native", "delegated", "delegated-stale"} {
		t.Run(mode, func(t *testing.T) {
			setup(t)
			data := t.TempDir()
			previous := callDesktopDirs
			callDesktopDirs = func() []string { return []string{data} }
			t.Cleanup(func() { callDesktopDirs = previous; Reset() })
			const desktopID = "11111111-aaaa-4000-8000-000000000001"
			writeDesktopTitle(t, filepath.Join(data, "claude-code-sessions", "account", "org", "local_desktop.json"),
				`{"cliSessionId":"`+desktopID+`","title":"Native Desktop title"}`)
			t.Setenv("PI_DESKTOP_DATA_DIR", t.TempDir())
			held := piDesktopTitlesHeld
			piDesktopTitlesHeld = 0
			t.Cleanup(func() { piDesktopTitlesHeld = held })
			open := func(path string, statements ...string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", filepath.ToSlash(path))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				for _, statement := range statements {
					if _, err := db.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			open(filepath.Join(PIDesktopDir(), "pi.sqlite"), piDesktopSchema,
				`INSERT INTO sessions (id, title, created_at, updated_at) VALUES ('pi-session', 'Native Pi title', 1, 2)`)
			open(zcodeTasksPath(),
				`CREATE TABLE tasks (task_id TEXT, title TEXT, updated_at INTEGER, deleted INTEGER)`,
				`INSERT INTO tasks VALUES ('sess_zcode-session', 'Native ZCode title', 1, 0)`)
			keys := []string{"claude-desktop:" + desktopID, "pi:pi-session", "pi-desktop:pi-session", "zcode:zcode-session"}
			if got := Titles(keys); len(got) != len(keys) {
				t.Fatal("native fixture did not provide all auxiliary titles", got)
			}
			if mode == "native" {
				return
			}
			bindings := []SessionSourceBinding{}
			for _, owner := range []string{"claude-desktop", "pi", "zcode"} {
				bindings = append(bindings, SessionSourceBinding{Plugin: "fixture", Source: "history",
					Agent: owner, Identity: strings.Repeat("a", 64)})
			}
			body, err := json.Marshal(bindings)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(sourceBindingsPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sourceBindingsPath(), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "delegated-stale" {
				sourceEpoch.Add(1)
			}
			// Direct callers (gateway metadata and Desktop) do not prepare
			// the SDK catalog first. A warm view must not hide foreign claims.
			if got := DesktopTitles([]string{desktopID}); len(got) != 0 {
				t.Fatal("direct Desktop title lookup bypassed delegated history owner", got)
			}
			if got := PIDesktopTitles([]string{"pi-session"}); len(got) != 0 {
				t.Fatal("direct Pi title lookup bypassed delegated history owner", got)
			}
			if got := Titles(keys); len(got) != 0 {
				t.Fatal("delegated history leaked titles from native auxiliary stores", got)
			}
		})
	}
}
