package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
)

func delegateSaidHistory(t *testing.T, agent string) {
	t.Helper()
	body, err := json.Marshal([]SessionSourceBinding{{
		Plugin: "fixture", Source: "history", Agent: agent, Identity: strings.Repeat("a", 64),
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeHistoryAuthority(t, sourceBindingsPath(), body)
}

func TestSaidIndexGrowthChecksHistoryOwner(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, authority := range []string{"delegated", "unreadable", "denied"} {
			t.Run(agent+"/"+authority, func(t *testing.T) {
				setup(t)
				line := `{"type":"user","message":{"role":"user","content":"privateindexword"}}`
				if agent == "codex" {
					line = cxSaid("user", "privateindexword")
				}
				session := saidFile(t, t.TempDir(), agent, "held", time.Now(), line)
				index := saidTestIndex(t, func() []saidSession { return []saidSession{session} })
				db, err := index.open()
				if err != nil {
					t.Fatal(err)
				}
				switch authority {
				case "delegated":
					delegateSaidHistory(t, agent)
				case "unreadable":
					writeHistoryAuthority(t, sourceBindingsPath(), []byte("{invalid"))
				case "denied":
					if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
						return &settings.SessionReads{Asked: true}
					}); err != nil {
						t.Fatal(err)
					}
				}
				scans := 0
				scanHook = func(path string, _ int64) {
					if path == session.s.Path {
						scans++
					}
				}
				t.Cleanup(func() { scanHook = nil })
				err = saidRead(db, agent+":held", session, 0, true)
				if err == nil || authority == "denied" && !errors.Is(err, ErrNotRead) ||
					authority != "denied" && !errors.Is(err, extension.ErrActionChanged) {
					t.Errorf("index read did not retain %s authority: %v", authority, err)
				}
				var rows int
				if err := db.QueryRow(`SELECT count(*) FROM files`).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if scans != 0 || rows != 0 {
					t.Errorf("unavailable history was read/persisted: scans=%d rows=%d", scans, rows)
				}
			})
		}
	}
}

func TestSaidIndexRevokedReadPreservesCommittedCache(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(map[bool]string{false: "append", true: "replace"}[whole], func(t *testing.T) {
			setup(t)
			dir := t.TempDir()
			original := `{"type":"user","message":{"role":"user","content":"previousword"}}`
			session := saidFile(t, dir, "claude", "held", time.Now(), original)
			index := saidTestIndex(t, func() []saidSession { return []saidSession{session} })
			db, err := index.open()
			if err != nil {
				t.Fatal(err)
			}
			if err := saidRead(db, "claude:held", session, 0, true); err != nil {
				t.Fatal(err)
			}
			offset := session.size
			writeLines(t, session.s.Path, original, `{"type":"user","message":{"role":"user","content":"newprivateword"}}`)
			session = saidOf(t, session.s)
			var once sync.Once
			scanHook = func(path string, _ int64) {
				if path == session.s.Path {
					once.Do(func() { delegateSaidHistory(t, "claude") })
				}
			}
			t.Cleanup(func() { scanHook = nil })
			if err := saidRead(db, "claude:held", session, offset, whole); !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("revocation while indexing was published: %v", err)
			}
			for _, word := range []string{"previousword", "newprivateword"} {
				keys, err := saidKeys(t.Context(), db, saidTerms(word))
				if err != nil {
					t.Fatal(err)
				}
				if keys["claude:held"] != (word == "previousword") {
					t.Errorf("failed refresh changed committed %s keys: %v", word, keys)
				}
			}
			var upto int64
			if err := db.QueryRow(`SELECT upto FROM files WHERE key = ?`, "claude:held").Scan(&upto); err != nil {
				t.Fatal(err)
			}
			if upto != offset {
				t.Errorf("failed refresh advanced cursor from %d to %d", offset, upto)
			}
		})
	}
}

func TestSearchSaidRechecksCollectedHits(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	now := time.Now()
	line := `{"type":"user","message":{"role":"user","content":"sharedprivateword"}}`
	first := saidFile(t, dir, "claude", "first", now, line)
	second := saidFile(t, dir, "claude", "second", now.Add(-time.Minute), line)
	index := saidTestIndex(t, func() []saidSession { return []saidSession{first, second} })
	warm, err := index.search(t.Context(), "sharedprivateword", "", 10)
	if err != nil || len(warm.Hits) != 2 {
		t.Fatalf("warm index fixture: hits=%d err=%v", len(warm.Hits), err)
	}
	scans := 0
	var once sync.Once
	scanHook = func(path string, _ int64) {
		if path == second.s.Path {
			scans++
			once.Do(func() { delegateSaidHistory(t, "claude") })
		}
	}
	t.Cleanup(func() { scanHook = nil })
	found, err := index.search(t.Context(), "sharedprivateword", "", 10)
	if scans == 0 {
		t.Fatal("probe did not revoke during the second selected read")
	}
	if !errors.Is(err, extension.ErrActionChanged) || len(found.Hits) != 0 {
		t.Fatalf("revoked burst published earlier hits: hits=%d err=%v", len(found.Hits), err)
	}
}

func TestSearchSaidUnknownAuthorityPreservesIndex(t *testing.T) {
	setup(t)
	session := saidFile(t, t.TempDir(), "claude", "held", time.Now(),
		`{"type":"user","message":{"role":"user","content":"retainedword"}}`)
	listings := 0
	index := saidTestIndex(t, func() []saidSession {
		listings++
		return []saidSession{session}
	})
	warm, err := index.search(t.Context(), "retainedword", "", 10)
	if err != nil || len(warm.Hits) != 1 {
		t.Fatalf("warm index fixture: %+v %v", warm, err)
	}
	writeHistoryAuthority(t, sourceBindingsPath(), []byte("{invalid"))
	index.stale()
	found, err := index.search(t.Context(), "retainedword", "", 10)
	if err == nil || len(found.Hits) != 0 {
		t.Errorf("unknown authority became a successful answer: %+v %v", found, err)
	}
	index.mu.Lock()
	run := index.running
	index.mu.Unlock()
	if run != nil {
		<-run
	}
	if listings != 1 {
		t.Errorf("unknown authority refreshed the cached listing: %d", listings)
	}
	keys, err := saidKeys(t.Context(), index.db, saidTerms("retainedword"))
	if err != nil || !keys["claude:held"] {
		t.Errorf("unknown authority destroyed the previous index: %v %v", keys, err)
	}
}

func TestSearchSaidCanceledDoesNotOpenIndex(t *testing.T) {
	setup(t)
	path := filepath.Join(t.TempDir(), "unused", "said.db")
	index := &saidIndex{where: func() string { return path }, list: func(context.Context) ([]saidSession, error) {
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	found, err := index.search(ctx, "anything", "", 10)
	if index.db != nil {
		t.Cleanup(func() {
			index.mu.Lock()
			run := index.running
			index.mu.Unlock()
			if run != nil {
				<-run
			}
			index.db.Close()
		})
	}
	if !errors.Is(err, context.Canceled) || len(found.Hits) != 0 {
		t.Errorf("canceled search returned content or no cancellation: %+v %v", found, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canceled search opened persistent index: %v", err)
	}
}

func TestSaidIndexFailedReadPreservesCommittedCache(t *testing.T) {
	setup(t)
	session := saidFile(t, t.TempDir(), "claude", "held", time.Now(),
		`{"type":"user","message":{"role":"user","content":"retainedword"}}`)
	index := saidTestIndex(t, func() []saidSession { return []saidSession{session} })
	db, err := index.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := saidRead(db, "claude:held", session, 0, true); err != nil {
		t.Fatal(err)
	}
	path := session.s.Path
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	session.grow = false
	if err := saidRead(db, "claude:held", session, 0, true); err == nil {
		t.Fatal("unreadable session was treated as an empty successful parse")
	}
	keys, err := saidKeys(t.Context(), db, saidTerms("retainedword"))
	if err != nil || !keys["claude:held"] {
		t.Fatalf("failed parse replaced committed cache: %v %v", keys, err)
	}
	var upto int64
	if err := db.QueryRow(`SELECT upto FROM files WHERE key = ?`, "claude:held").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	if upto != session.size {
		t.Fatalf("failed parse changed committed cursor: %d want %d", upto, session.size)
	}
}

func TestSearchSaidCatalogFailurePreservesCommittedCache(t *testing.T) {
	setup(t)
	session := saidFile(t, t.TempDir(), "claude", "held", time.Now(),
		`{"type":"user","message":{"role":"user","content":"retainedword"}}`)
	listings := 0
	index := saidTestIndex(t, func() []saidSession {
		listings++
		return []saidSession{session}
	})
	previous := saidIdx
	saidIdx = index
	t.Cleanup(func() { saidIdx = previous })
	warm, err := SearchSaid(t.Context(), "retainedword", "", 10)
	if err != nil || len(warm.Hits) != 1 {
		t.Fatalf("ordinary search fixture: %+v %v", warm, err)
	}
	writeHistoryAuthority(t, extension.CatalogPath(), []byte("{invalid"))
	index.stale()
	found, err := SearchSaid(t.Context(), "retainedword", "", 10)
	if err == nil || len(found.Hits) != 0 {
		t.Fatalf("unreadable catalog became a successful answer: %+v %v", found, err)
	}
	if listings != 1 {
		t.Fatalf("unreadable catalog refreshed a listing: %d", listings)
	}
	keys, err := saidKeys(t.Context(), index.db, saidTerms("retainedword"))
	if err != nil || !keys["claude:held"] {
		t.Fatalf("unreadable catalog discarded the last-good index: %v %v", keys, err)
	}
}
