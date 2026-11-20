package sessions

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

func TestSharedPiOMPTraceOwnership(t *testing.T) {
	fixture := "testdata/omp/agent/sessions/--work-omp--/2026-09-28T08-00-00-000Z_" + ompMain + ".jsonl"
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	title, _, _ := bytes.Cut(body, []byte{'\n'})
	for _, mode := range []string{"both", "pi", "omp"} {
		t.Run(mode, func(t *testing.T) {
			setup(t)
			root := t.TempDir()
			t.Setenv("PI_CODING_AGENT_DIR", root)
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
			t.Setenv("OMP_PROFILE", "")
			piAllowed, ompAllowed := mode != "omp", mode != "pi"
			err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
				return &settings.SessionReads{Asked: true, Agents: map[string]bool{"pi": piAllowed, "omp": ompAllowed}}
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			write := func(owner string, prefix ...string) {
				path := filepath.Join(root, "sessions", "work", "today_"+owner+".jsonl")
				writeLines(t, path, append(prefix,
					fmt.Sprintf(`{"type":"session","id":%q}`, owner),
					fmt.Sprintf(`{"type":"message","id":"live","timestamp":%q,"message":{"role":"user","content":"prompt"}}`, now.Format(time.RFC3339Nano)),
				)...)
			}
			write("pi")
			write("omp", string(title))

			var want []string
			if ompAllowed {
				want = append(want, "omp/omp")
			}
			if piAllowed {
				want = append(want, "pi/pi")
			}
			reader := NewTraceReader(now.Add(-time.Second))
			var got []string
			for _, span := range reader.Poll(false) {
				if span.Turn == "live" {
					got = append(got, span.Agent+"/"+span.Session)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("shared-store traces = %v, want %v", got, want)
			}
			if spans := reader.Poll(false); len(spans) != 0 {
				t.Fatalf("shared store replayed unchanged traces: %+v", spans)
			}
		})
	}
}

func TestNativeTraceFollowsHistoryAuthority(t *testing.T) {
	for _, phase := range []string{"delegated", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			_, codex := setup(t)
			root := filepath.Join(codex, "isolated")
			t.Setenv("CODEX_HOME", root)
			const id = "99999999-aaaa-4000-8000-000000000001"
			path := filepath.Join(root, "sessions", "2026", "11", "19", "rollout-2026-11-19T15-00-00-"+id+".jsonl")
			now := time.Now().UTC()
			write := func(turn string) {
				writeLines(t, path,
					`{"type":"session_meta","payload":{"id":"`+id+`"}}`,
					`{"timestamp":"`+now.Format(time.RFC3339Nano)+`","type":"event_msg","payload":{"type":"task_started","turn_id":"`+turn+`"}}`)
			}
			index := new(TraceSessionIndex)
			reader := NewTraceReader(now.Add(-time.Second))
			reader.SetSessionIndex(index)
			write("first")
			if spans := reader.Poll(true); len(spans) != 1 || spans[0].Turn != "first" ||
				!index.Visible("codex", id) || !index.VisibleFresh("codex", id) {
				t.Fatalf("native trace or identity missing: %+v", spans)
			}

			if phase == "delegated" {
				delegateHistoryForTest(t, "codex")
			} else {
				writeHistoryAuthority(t, sourceBindingsPath(), []byte("{invalid"))
			}
			if index.Visible("codex", id) {
				t.Error("warm identity survived loss of native authority")
			}
			if index.VisibleFresh("codex", id) {
				t.Error("fresh discovery read a native identity without authority")
			}
			write("next-longer-turn")
			if spans := reader.Poll(true); len(spans) != 0 || len(reader.VisibleSessions()) != 0 {
				t.Errorf("warm reader published a native trace or identity without authority: %+v, %+v", spans, reader.VisibleSessions())
			}
			cold := NewTraceReader(now.Add(-time.Second))
			if spans := cold.Poll(true); len(spans) != 0 || len(cold.VisibleSessions()) != 0 {
				t.Errorf("cold reader published a native trace or identity without authority: %+v, %+v", spans, cold.VisibleSessions())
			}
		})
	}
}
