package sessions

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestNativeCallAccountingSurvivesSourceRefresh(t *testing.T) {
	fixture := setupCalls(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	path := filepath.Join(fixture.claude, "projects", "-p", "sess1.jsonl")
	writeLines(t, path, claudeMsg("m1", "claude-sonnet-5", 1, 2, 3, 4, 1))

	previous := readingCalls
	t.Cleanup(func() { readingCalls = previous })
	readingCalls = func() {
		if err := PrepareSessionSources(t.Context()); err != nil {
			t.Errorf("refresh session sources: %v", err)
		}
	}
	if calls := Calls(time.Time{}); len(calls) != 1 || calls[0].Input != 1 {
		t.Fatalf("source refresh discarded native accounting: %+v", calls)
	}

	readingCalls = func() { delegateHistoryForTest(t, "claude") }
	if calls := Calls(time.Time{}); len(calls) != 0 {
		t.Fatalf("source adoption published the previous owner's calls: %+v", calls)
	}
}

func TestNativeCallAccountingFollowsHistoryAuthority(t *testing.T) {
	for _, mode := range []string{"delegated", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			fixture := setupCalls(t)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
			const id = "019a1111-2222-7333-8444-555566667777"
			path := codexIdentityRollout(t, fixture.codex, id,
				`{"type":"session_meta","payload":{"id":"`+id+`","source":"cli","cwd":"/work/codex-owned"}}`,
				codexIdentityUsage(10, "owned-call", id, id))
			source, identities := CallSourcesAndCodexIdentities()
			index := slices.IndexFunc(source, func(s CallSource) bool { return s.Path == path })
			if index < 0 || identities[id].ID != id || CodexThreadCwd(id) != "/work/codex-owned" {
				t.Fatalf("native accounting or thread metadata missing: %+v", identities[id])
			}
			held := source[index]
			hasCall := func(calls []Call) bool {
				return slices.ContainsFunc(calls, func(c Call) bool { return c.ResponseID == "owned-call" })
			}
			if !hasCall(ReadCallSource(held)) || !hasCall(AgentCalls(time.Time{}, "codex")) {
				t.Fatal("native call fixture did not populate the accounting readers")
			}

			if mode == "delegated" {
				delegateHistoryForTest(t, "codex")
			} else {
				writeHistoryAuthority(t, sourceBindingsPath(), []byte("{invalid"))
			}
			for _, sources := range [][]CallSource{CallSources(), func() []CallSource {
				sources, ids := CallSourcesAndCodexIdentities()
				if _, ok := ids[id]; ok {
					t.Error("combined usage discovery published a revoked thread identity")
				}
				return sources
			}()} {
				if slices.ContainsFunc(sources, func(s CallSource) bool { return s.Path == path }) {
					t.Error("revoked source still listed for accounting")
				}
			}
			if hasCall(ReadCallSource(held)) || hasCall(Calls(time.Time{})) || hasCall(AgentCalls(time.Time{}, "codex")) {
				t.Error("a held or cold accounting reader returned a revoked call")
			}
			if _, ok := CodexIdentities()[id]; ok || CodexThreadCwd(id) != "" {
				t.Error("direct lookup published revoked Codex metadata")
			}
		})
	}
}
