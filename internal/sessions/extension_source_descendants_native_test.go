package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
)

func TestSessionSourceOMPDescendantsNative(t *testing.T) {
	distribution, fixtures := os.Getenv("MAGPIE_OMP_TEST_DIST"), os.Getenv("MAGPIE_OMP_TEST_FIXTURES")
	if distribution == "" || fixtures == "" {
		t.Skip("requires an explicitly built OMP native distribution and synthetic fixtures")
	}
	consentHome(t)
	if err := SetReads(nil, map[string]bool{"omp": true, "codex": true}); err != nil {
		t.Fatal(err)
	}
	previous := agentThere
	agentThere = func(agent string) bool { return agent == "codex" }
	t.Cleanup(func() { agentThere = previous })
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	installed, err := manager.Install(t.Context(), distribution)
	if err != nil {
		t.Fatal(err)
	}
	config, err := manager.ModuleConfig(t.Context(), installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	allowed := json.RawMessage(`{"sessionSource":{"ownOff":false,"records":[{"sourceId":"omp-history","owned":true,"enabled":true}]}}`)
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), installed.Manifest.ID, config.Version, allowed); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(fixtures, "omp-source-usage-v3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const mainID = "019a0000-0000-7000-8000-00000000000a"
	const childID = "019a0000-0000-7000-8000-00000000001a"
	mainPath := filepath.Join(OmpDir(), "sessions", "--work-omp--", "time_"+mainID+".jsonl")
	childPath := strings.TrimSuffix(mainPath, ".jsonl") + "/nested/child.jsonl"
	write := func(name string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(mainPath, journal)
	child := []byte(`{"type":"session","version":3,"id":"` + childID + `","timestamp":"2026-10-25T03:50:25.123Z","cwd":"/work/child"}
{"type":"message","id":"u","parentId":null,"timestamp":"2026-10-25T03:50:26.123Z","message":{"role":"user","content":"private descendant question"}}
{"type":"message","id":"a","parentId":"u","timestamp":"2026-10-25T03:50:27.123Z","message":{"role":"assistant","model":"child-model","usage":{"input":31,"output":3},"content":"private descendant answer"}}
`)
	if err := BindSessionSourceNative(t.Context(), installed.Manifest.ID, "omp-history", installed.Revision); err != nil {
		t.Fatal(err)
	}
	solo, ok := Find("omp", mainID)
	if !ok || solo.source == nil || solo.Input != 130 || len(solo.Subs) != 0 {
		t.Fatal("ordinary Source without descendants changed its measurements", solo, SessionSourceErrors())
	}
	write(childPath, child)
	write(strings.TrimSuffix(mainPath, ".jsonl")+"/btw-history/ignored.jsonl", []byte("{invalid}\n"))
	if _, err := TranscriptOf(solo); err == nil {
		t.Fatal("new child tree retained old main-only snapshot authority")
	}
	read := func(input int64) Session {
		t.Helper()
		session, ok := Find("omp", mainID)
		if !ok || session.source == nil || session.SourceStatus != "ready" || int64(session.Input) != input ||
			!slices.Equal(session.Subs, []string{childID}) {
			t.Fatal("ordinary Source lost descendant usage or gateway identities", session, SessionSourceErrors())
		}
		return session
	}
	session := read(161)
	aliasPath := filepath.Join(filepath.Dir(mainPath), "time_"+childID+".jsonl")
	write(aliasPath, bytes.ReplaceAll(journal, []byte(mainID), []byte(childID)))
	if _, ok := Find("omp", mainID); ok {
		t.Fatal("descendant identity collided with another ordinary source session")
	}
	if _, ok := Find("omp", childID); ok {
		t.Fatal("ambiguous main/descendant identity was published")
	}
	if err := os.Remove(aliasPath); err != nil {
		t.Fatal(err)
	}
	session = read(161)
	listed := List(0)
	if count(listed, "omp") != 1 || len(MergeExternal(listed, []ExternalSession{{Agent: "omp", ID: childID}})) != len(listed) {
		t.Fatal("descendant became another ordinary or gateway session")
	}
	ids := NativeIDs(map[string]bool{"omp": true})["omp"]
	_, native := StatsAndNative(0)
	if !ids[childID] || !native["omp|"+childID] {
		t.Fatal("descendant usage is counted again by ordinary stats", ids, native)
	}
	transcript, err := TranscriptOf(session)
	body, _ := json.Marshal(transcript)
	if err != nil || !bytes.Contains(body, []byte("Recovered summary")) || bytes.Contains(body, []byte("private descendant")) {
		t.Fatal("descendant history was flattened into the main transcript", err, string(body))
	}
	if _, found, err := SearchSession(t.Context(), session, NeedleOf("private descendant")); err != nil || found {
		t.Fatal("ordinary main search exposed a descendant conversation", found, err)
	}
	carry, err := CarryOver(session, "codex")
	if err != nil || carry.Agent != "codex" {
		t.Fatal("ordinary Source/Codex conversion regressed", carry, err)
	}
	exported, err := os.ReadFile(carry.Path)
	if err != nil || bytes.Contains(exported, []byte("private descendant")) {
		t.Fatal("ordinary carry fabricated descendant user turns", err)
	}
	child = bytes.Replace(child, []byte(`"input":31`), []byte(`"input":37`), 1)
	write(childPath, child)
	if _, err := TranscriptOf(session); err == nil {
		t.Fatal("unchanged main journal retained changed child authority")
	}
	session = read(167)
	write(childPath, []byte("{invalid}\n"))
	stale, ok := Find("omp", mainID)
	if !ok || stale.SourceStatus != "stale" || stale.Input != session.Input {
		t.Fatal("malformed child erased last-good usage", stale, SessionSourceErrors())
	}
	if _, err := TranscriptOf(stale); err == nil {
		t.Fatal("malformed child kept transcript authority")
	}
	write(childPath, child)
	session = read(167)
	lateMutation := sync.OnceFunc(func() { write(childPath, []byte("{invalid}\n")) })
	sourceBeforePublish = lateMutation
	t.Cleanup(func() { sourceBeforePublish = nil })
	late, ok := Find("omp", mainID)
	sourceBeforePublish = nil
	if !ok || late.SourceStatus != "stale" || late.Input != session.Input {
		t.Fatal("child changed before publication retained ready authority or erased last-good metadata", late)
	}
	if _, err := TranscriptOf(late); err == nil {
		t.Fatal("child changed before publication retained transcript authority")
	}
	write(childPath, child)
	session = read(167)
	if err := SetReads(nil, map[string]bool{"omp": false}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("omp", mainID); ok {
		t.Fatal("revoked consent exposed delegated descendants")
	}
	if _, err := CarryOver(session, "codex"); err == nil {
		t.Fatal("revoked consent retained child-backed carry authority")
	}
	if err := SetReads(nil, map[string]bool{"omp": true}); err != nil {
		t.Fatal(err)
	}
	session = read(167)
	if err := manager.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("omp", mainID); ok {
		t.Fatal("disabled source fell through to native parser")
	}
	if _, err := TranscriptOf(session); err == nil {
		t.Fatal("disabled source retained descendant authority")
	}
	after, err := os.ReadFile(mainPath)
	if err != nil || !bytes.Equal(after, journal) {
		t.Fatal("ordinary acquisition or conversion modified the source journal", err)
	}
}
