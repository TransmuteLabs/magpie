package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ompMain  = "019a0000-0000-7000-8000-00000000000a"
	ompFork  = "019a0000-0000-7000-8000-00000000000c"
	ompNamed = "019a0000-0000-7000-8000-00000000000d"
)

func TestOmpConfigDirSessionSource(t *testing.T) {
	for _, mode := range []string{"relative", "absolute"} {
		t.Run(mode, func(t *testing.T) {
			setup(t)
			t.Setenv("PI_CODING_AGENT_DIR", "")
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			value := ".omp-work"
			root := filepath.Join(home, value)
			if mode == "absolute" {
				root = filepath.Join(t.TempDir(), "omp work")
				value = root
			}
			t.Setenv("PI_CONFIG_DIR", value)
			agentDir := filepath.Join(root, "profiles", "work", "agent")
			sessionsDir := filepath.Join(agentDir, "sessions")
			copyTree(t, "testdata/omp/agent/sessions", sessionsDir)

			readAll := false
			setRead := func(allowed bool) {
				t.Helper()
				if err := SetReads(&readAll, map[string]bool{"omp": allowed}); err != nil {
					t.Fatal(err)
				}
			}
			setRead(false)
			if _, found := Find("omp", ompMain); found {
				t.Fatal("OMP journal listed without read consent")
			}
			if !slices.Contains(Found(), "omp") {
				t.Fatal("configured OMP store unavailable for consent")
			}

			setRead(true)
			const relative = "sessions/--work-omp--/2026-09-28T08-00-00-000Z_" + ompMain + ".jsonl"
			session, found := Find("omp", ompMain)
			if !found || session.Path != filepath.Join(agentDir, filepath.FromSlash(relative)) ||
				session.Title != "Port the parser to omp" ||
				!strings.Contains(session.Resume, "omp --profile work --resume "+ompMain) {
				t.Fatalf("OMP did not select the configured profile journal: %+v", session)
			}
			source := sdk.SessionSourceDescriptor{Agent: "omp"}
			slot := sdk.SessionSourceStore{Slot: "main", NativeRootSlot: "omp.sessions"}
			if roots, err := sourceStoreRoots(source, slot); err != nil || !slices.Contains(roots, sessionsDir) {
				t.Fatalf("configured profile missing from native Source roots: %v, %v", roots, err)
			}
			physical, err := filepath.EvalSymlinks(agentDir)
			if err != nil {
				t.Fatal(err)
			}
			if dir, path, err := sessionSourceRoot(session); err != nil || dir != physical || path != relative {
				t.Fatalf("native source input = (%q, %q, %v), want (%q, %q)", dir, path, err, physical, relative)
			}
			if transcript, err := TranscriptOf(session); err != nil ||
				len(transcript.Parts) == 0 || transcript.Parts[0].Text != "Port the parser" {
				t.Fatalf("configured OMP transcript unavailable: %v", err)
			}

			setRead(false)
			if _, found := Find("omp", ompMain); found {
				t.Fatal("OMP journal listed after consent was revoked")
			}
			if _, err := TranscriptOf(session); !errors.Is(err, ErrNotRead) {
				t.Fatalf("retained OMP session read after consent was revoked: %v", err)
			}
		})
	}
}

// omp's sessions (#303): Pi's kind, with its title line and title changes,
// model_usage entries, and its subagents' and advisor's sessions beside it.
func TestOmp(t *testing.T) {
	inZone(t, 0)
	home := setupAgents(t)
	ss := List(0)
	if n := count(ss, "omp"); n != 3 {
		t.Fatalf("want 3 omp sessions (the subagents and advisor in theirs), got %d", n)
	}
	m := find(t, ss, "omp", ompMain)
	if m.Cwd != "/work/omp" || m.Title != "Port the parser to omp" {
		t.Fatalf("omp: %+v", m)
	}
	// the task tool's summed usage not counted again
	for _, c := range []struct {
		model string
		want  Tokens
	}{
		{"claude-opus-5-5", Tokens{110, 55, 1500, 200, 0}},
		{"claude-haiku-5", Tokens{20, 5, 0, 0, 0}},      // a model_usage entry
		{"claude-sonnet-5", Tokens{340, 34, 100, 0, 0}}, // the subagent and its own
		{"gpt-6-astra", Tokens{50, 10, 0, 0, 0}},        // the advisor
	} {
		if got := model(m, c.model); got.Tokens != c.want {
			t.Fatalf("%s: %+v, want %+v", c.model, got, c.want)
		}
	}
	if len(m.Models) != 4 || !model(m, "claude-opus-5-5").Priced {
		t.Fatalf("models %+v", m.Models)
	}
	if !m.Start.Equal(at("2026-09-28T08:00:00Z")) || !m.Last.Equal(at("2026-09-28T08:02:00Z")) {
		t.Fatalf("omp times %s %s", m.Start, m.Last)
	}
	want := "cd '/work/omp' && OMP_PROFILE=default omp --resume " + ompMain
	if runtime.GOOS == "windows" {
		want = "Set-Location -LiteralPath '/work/omp'; " +
			"& { $savedOMPProfile = $env:OMP_PROFILE; try { $env:OMP_PROFILE = 'default'; omp --resume " +
			ompMain + " } finally { $env:OMP_PROFILE = $savedOMPProfile } }"
	}
	if m.Resume != want {
		t.Fatalf("resume %q", m.Resume)
	}

	// the fork: what it copied counted in the session it came from
	f := find(t, ss, "omp", ompFork)
	if f.Tokens != (Tokens{7, 3, 0, 0, 0}) || f.Title != "Port the parser" {
		t.Fatalf("fork: %+v", f)
	}
	if !f.Start.Equal(at("2026-09-29T09:00:00Z")) || !f.Last.Equal(at("2026-09-29T09:00:20Z")) {
		t.Fatalf("fork times %s %s", f.Start, f.Last)
	}

	// begun from a template: the title it was last given
	if n := find(t, ss, "omp", ompNamed); n.Title != "Tidy the tests" {
		t.Fatalf("named: %+v", n)
	}

	// a profile's sessions, and those in $XDG_DATA_HOME/omp
	from := filepath.Join(home, ".omp", "agent", "sessions", "--work-omp--")
	moves := map[string]string{ompNamed: filepath.Join(home, ".omp", "profiles", "work", "agent", "sessions", "--work-omp--")}
	if runtime.GOOS != "windows" {
		moves[ompFork] = filepath.Join(os.Getenv("XDG_DATA_HOME"), "omp", "sessions", "--work-omp--")
	}
	for id, to := range moves {
		names, _ := filepath.Glob(filepath.Join(from, "*_"+id+".jsonl"))
		if len(names) != 1 {
			t.Fatalf("fixture for %s: %v", id, names)
		}
		os.MkdirAll(to, 0o755)
		if err := os.Rename(names[0], filepath.Join(to, filepath.Base(names[0]))); err != nil {
			t.Fatal(err)
		}
	}
	Reset()
	ss = List(0)
	for id := range moves {
		m := find(t, ss, "omp", id)
		if id == ompNamed && !strings.Contains(m.Resume, "omp --profile work --resume "+id) {
			t.Fatalf("profile resume %q", m.Resume)
		}
	}
}

// Jumy (Discord, 10-30): omp's subagents and advisor each send their own
// session id to the gateway (x-session-affinity, the id in their file's
// header), not the main session's. Their files are counted in the main
// session, so the gateway's records under those ids are the same calls:
// they are the session's own to the gateway, not sessions of their own
// counted again.
func TestOmpSubagentsAreTheSessionsToTheGateway(t *testing.T) {
	setupAgents(t)
	subs := []string{
		"019a0000-0000-7000-8000-00000000001a", // 0-Explore.jsonl
		"019a0000-0000-7000-8000-00000000002a", // 0-Explore/0-Find.jsonl
		"019a0000-0000-7000-8000-00000000003a", // __advisor.jsonl
	}
	ss := List(0)
	m := find(t, ss, "omp", ompMain)
	keys := GatewayKeys(m)
	for _, id := range subs {
		if !slices.Contains(keys, "omp|"+id) {
			t.Errorf("GatewayKeys leave out the subagent %s: %v", id, keys)
		}
	}
	merged := MergeExternal(ss, []ExternalSession{
		{Agent: "omp", ID: subs[0], Title: "fail"},
		{Agent: "omp", ID: subs[2], Title: "fail"},
		{Agent: "omp", ID: "01a18ae4-0000-7000-8000-000000000000", Title: "omp's own"},
	})
	if n, want := count(merged, "omp"), count(ss, "omp")+1; n != want {
		t.Fatalf("the gateway's records of omp's subagents listed as sessions: %d omp sessions, want %d", n, want)
	}
	ids := NativeIDs(map[string]bool{"omp": true})["omp"]
	_, native := StatsAndNative(0)
	for _, id := range append([]string{ompMain}, subs...) {
		if !ids[id] {
			t.Errorf("omp's native ids leave out %s: %v", id, ids)
		}
		if !native["omp|"+id] {
			t.Errorf("the gateway's omp session %s is counted again: %v", id, native)
		}
	}
	// the other sessions keep only their own
	for _, id := range []string{ompFork, ompNamed} {
		if s := find(t, ss, "omp", id); len(s.Subs) != 0 {
			t.Errorf("%s: subagents %v", id, s.Subs)
		}
	}
}
