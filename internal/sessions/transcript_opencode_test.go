package sessions

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptOpenCodeV2(t *testing.T) {
	data, _ := setupMore(t)
	db := ocMakeV2(t, filepath.Join(data, "opencode", "opencode.db"))
	defer db.Close()
	s := find(t, List(All), "opencode", ocMain)
	tr, err := TranscriptOf(s)
	if err != nil || len(tr.Parts) != 4 {
		t.Fatalf("OpenCode conversation parts=%d error=%v", len(tr.Parts), err)
	}
	if tr.Parts[0].Role != "user" || tr.Parts[3].Text != "It races the clock." {
		t.Fatalf("wrong conversation order: %+v", tr)
	}
}

func TestTranscriptOpenCodeFiles(t *testing.T) {
	setupMore(t)
	s := find(t, List(All), "opencode", ocMain)
	tr, err := TranscriptOf(s)
	if err != nil || len(tr.Parts) == 0 {
		t.Fatalf("legacy OpenCode conversation parts=%d error=%v", len(tr.Parts), err)
	}
}

func TestSearchSaidOpenCodeSkipsToolState(t *testing.T) {
	data, _ := setupMore(t)
	db := ocMakeV2(t, filepath.Join(data, "opencode", "opencode.db"))
	defer db.Close()
	s := find(t, List(All), "opencode", ocMain)
	payload := strings.Repeat("opencodetooloutput ", 64<<10)
	raw := `{"model":{"id":"codex/gpt-6-luna"},"content":[
		{"type":"text","text":"kept before tool"},
		{"type":"tool","name":"shell","state":{"input":{"command":"run"},"output":"` + payload + `"}},
		{"type":"text","text":"kept after tool"},
		{"type":"text","text":"excludedsynthetic","synthetic":true},
		{"type":"text","text":"excludedignored","ignored":true},
		{"type":"text","text":"excludedinvalid","state":{"output":23}},
		{"type":"text","text":"<excludedcontext>"}]}`
	if _, err := db.Exec("INSERT INTO session_message VALUES (?, ?, ?, ?, ?, ?, ?)", "msg_large", s.ID, "assistant", 100, 1790416980000, 1790416980000, raw); err != nil {
		t.Fatal(err)
	}
	session := saidSession{s: s, size: 5}
	index := saidTestIndex(t, func() []saidSession { return []saidSession{session} })
	fts, err := index.open()
	if err != nil {
		t.Fatal(err)
	}
	indexBytes := allocated(3, func() {
		if err := saidRead(fts, "opencode:"+s.ID, session, 0, true); err != nil {
			t.Fatal(err)
		}
	})
	var spoken []string
	fullBytes := allocated(3, func() {
		spoken = spoken[:0]
		if err := readTranscript(s, func(_ bool, p Part) bool {
			if said(p) && strings.HasPrefix(p.Text, "kept ") {
				spoken = append(spoken, p.Text)
				if p.Model != "codex/gpt-6-luna" {
					t.Errorf("reply model = %q", p.Model)
				}
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("index allocated %d bytes, full decode %d bytes", indexBytes, fullBytes)
	if indexBytes+uint64(len(payload)) >= fullBytes {
		t.Errorf("index still allocates the tool state")
	}
	if got := strings.Join(spoken, ", "); got != "kept before tool, kept after tool" {
		t.Errorf("reply order = %q", got)
	}
	for needle, want := range map[string]int{
		"kept before": 1, "kept after": 1, "opencodetooloutput": 0,
		"excludedsynthetic": 0, "excludedignored": 0, "excludedinvalid": 0, "excludedcontext": 0,
	} {
		got, err := saidKeys(t.Context(), fts, saidTerms(NeedleOf(needle)))
		if err != nil || len(got) != want {
			t.Errorf("index %q: keys %v, want %d, error %v", needle, got, want, err)
		}
	}
	hit, found, err := SearchSession(t.Context(), s, NeedleOf("opencodetooloutput"))
	if err != nil || !found || hit.Kind != "tool_result" {
		t.Fatalf("direct tool search: %+v, found %t, error %v", hit, found, err)
	}
}
