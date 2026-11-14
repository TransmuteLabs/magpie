package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
)

func installedSourceFixture(t *testing.T) (*extension.Manager, extension.Installed, string) {
	t.Helper()
	distribution := os.Getenv("MAGPIE_SOURCE_TEST_DIST")
	if distribution == "" {
		t.Skip("requires explicitly built synthetic native Source 1.6 fixture")
	}
	consentHome(t)
	if err := SetReads(nil, map[string]bool{"fixture-source": true, "omp": true}); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	entry, err := manager.Install(t.Context(), distribution)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture-source", "sessions", "sess.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFixtureWrite(t, path, "first", 7)
	return manager, entry, path
}

func sourceFixtureWrite(t *testing.T, path, text string, input uint64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": strings.TrimSuffix(filepath.Base(path), ".jsonl"), "cwd": "/work/source",
		"title": text, "at": "2026-11-13T00:00:00Z", "role": "user", "text": text, "input": input})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSourceNativeOrdinaryReadAndCarry(t *testing.T) {
	manager, installed, path := installedSourceFixture(t)
	previous := agentThere
	agentThere = func(agent string) bool { return agent == "omp" }
	t.Cleanup(func() { agentThere = previous })
	original, _ := os.ReadFile(path)
	listed := List(0)
	index := slices.IndexFunc(listed, func(session Session) bool { return session.Agent == "fixture-source" })
	if index < 0 {
		t.Fatal("ordinary List did not discover native source", listed)
	}
	session := listed[index]
	if session.ID != "sess" || session.Cwd != "/work/source" || session.Input != 7 ||
		session.Path != path || !strings.Contains(session.Title, fmt.Sprintf("pid:%d", os.Getpid())) {
		t.Fatal("native PID/identity/usage projection failed", session)
	}
	if session.Resume != "" || session.SourceStatus != "ready" {
		t.Fatal("unknown native launcher was fabricated", session)
	}
	if len(session.source.request.Documents) != 0 {
		t.Fatal("source body retained after summary")
	}
	for _, find := range []func() (Session, bool){
		func() (Session, bool) { return Find("fixture-source", "sess") },
		func() (Session, bool) { return Get("fixture-source:sess") },
	} {
		found, ok := find()
		if !ok || found.Path != path || found.Input != 7 {
			t.Fatal("ordinary source lookup lost selection", found)
		}
	}
	if title := Titles([]string{"fixture-source:sess"})["fixture-source:sess"]; title != session.Title {
		t.Fatal("source title missing", title)
	}
	if count := len(ListAgent("fixture-source")); count != 1 {
		t.Fatal("source absent from management", count)
	}
	if ids := NativeIDs(map[string]bool{"fixture-source": true}); !ids["fixture-source"]["sess"] {
		t.Fatal("source identity absent from native set", ids)
	}
	transcript, err := TranscriptOf(session)
	if err != nil || len(transcript.Parts) != 1 || transcript.Parts[0].Text != "first" {
		t.Fatal("ordinary source transcript failed", transcript, err)
	}
	hit, found, err := SearchSession(t.Context(), session, NeedleOf("first"))
	if err != nil || !found || hit.Match == "" {
		t.Fatal("ordinary source search failed", hit, found, err)
	}
	stats := StatsFor(0)
	if !slices.ContainsFunc(stats.Sessions, func(summary Summary) bool { return summary.Agent == "fixture-source" && summary.Input == 7 }) {
		t.Fatal("source usage bypassed ordinary stats", stats.Sessions)
	}
	mu.Lock()
	if saved := savable(); saved[path] != nil {
		mu.Unlock()
		t.Fatal("source cache was persisted as a native path parse")
	}
	mu.Unlock()
	result, err := CarryOver(session, "omp")
	if err != nil || result.ID != "converted-source" || !strings.Contains(result.Command, "omp --resume converted-source") {
		t.Fatal("ordinary source conversion failed", result, err)
	}
	converted, found := Find("omp", result.ID)
	if !found || converted.Path != result.Path {
		t.Fatal("converted native output did not enter ordinary native discovery", converted)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(original, after) {
		t.Fatal("source conversion changed original")
	}
	if err := manager.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("fixture-source", "sess"); ok {
		t.Fatal("disabled source remained listed")
	}
	if _, err := TranscriptOf(session); err == nil {
		t.Fatal("old session retained read authority after disable")
	}
}

func TestSourceNativeLifecycle(t *testing.T) {
	manager, installed, path := installedSourceFixture(t)
	session, ok := Find("fixture-source", "sess")
	if !ok {
		t.Fatal("source missing")
	}
	sourceFixtureWrite(t, path, "changed", 11)
	changed, ok := Find("fixture-source", "sess")
	if !ok || changed.Input != 11 || strings.Contains(changed.Title, "first") {
		t.Fatal("rewritten source accumulated old usage", changed)
	}
	if _, err := TranscriptOf(session); err == nil {
		t.Fatal("old digest was substituted with changed source")
	}
	if err := os.WriteFile(path, []byte("{invalid}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, ok := Find("fixture-source", "sess")
	if !ok || stale.SourceStatus != "stale" || stale.Input != 11 {
		t.Fatal("read failure became empty/zero instead of last-good", stale)
	}
	if _, err := TranscriptOf(stale); err == nil || ResumeNow(stale) != "" {
		t.Fatal("stale source retained action authority")
	}
	sourceFixtureWrite(t, path, "restored", 13)
	restored, ok := Find("fixture-source", "sess")
	if !ok || restored.Input != 13 || restored.SourceStatus != "ready" {
		t.Fatal("source failed to recover", restored)
	}
	config, err := manager.ModuleConfig(t.Context(), installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), installed.Manifest.ID, config.Version, json.RawMessage(`{"test":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := TranscriptOf(restored); err == nil {
		t.Fatal("config change retained old source authority")
	}
	if _, ok := Find("fixture-source", "sess"); !ok {
		t.Fatal("new config generation did not recover source")
	}
	if err := SetReads(nil, map[string]bool{"fixture-source": false}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("fixture-source", "sess"); ok {
		t.Fatal("source ignored revoked read consent")
	}
	if _, err := TranscriptOf(restored); !errors.Is(err, ErrNotRead) {
		t.Fatal("old source bypassed read consent", err)
	}
	if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
		return &settings.SessionReads{Asked: true, All: true}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceNativeOwnershipRequiresBinding(t *testing.T) {
	manager, installed, _ := installedSourceFixture(t)
	copyTree(t, "testdata/omp/agent", OmpDir())
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture-omp", "sessions", "native-id.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFixtureWrite(t, path, "native", 3)
	if _, ok := Find("omp", "native-id"); ok {
		t.Fatal("module took native owner without host binding")
	}
	if _, ok := Find("omp", ompMain); !ok {
		t.Fatal("source install changed default native owner")
	}
	if err := BindSessionSourceNative(t.Context(), installed.Manifest.ID, "native-omp", "wrong"); err == nil {
		t.Fatal("binding ignored expected package revision")
	}
	if err := BindSessionSourceNative(t.Context(), installed.Manifest.ID, "native-omp", installed.Revision); err != nil {
		t.Fatal(err)
	}
	session, ok := Find("omp", "native-id")
	if !ok || session.source == nil {
		t.Fatal("checked native binding did not register reader", session)
	}
	if !strings.Contains(resumeCommand("", "omp", session.ID, session.Cwd, session.Path), "omp --resume native-id") {
		t.Fatal("host native resume recipe was not used")
	}
	if !strings.Contains(session.Resume, "omp --resume native-id") || ResumeNow(session) != session.Resume {
		t.Fatal("ordinary source resume bypassed host native recipe", session.Resume)
	}
	if err := manager.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("omp", "native-id"); ok {
		t.Fatal("disabled delegated source fell through to native default parser")
	}
	if _, ok := Find("omp", ompMain); ok {
		t.Fatal("disabled binding silently restored a different native owner")
	}
	bindings, err := readSourceBindings()
	if err != nil || len(bindings) != 1 {
		t.Fatal("checked binding was lost", bindings, err)
	}
	if err := UnbindSessionSourceNative(t.Context(), "omp", "wrong"); err == nil {
		t.Fatal("native ownership release ignored expected identity")
	}
	if err := UnbindSessionSourceNative(t.Context(), "omp", bindings[0].Identity); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("omp", ompMain); !ok {
		t.Fatal("explicit native ownership release failed to restore default")
	}
}

func TestSourceNativePreparationDoesNotHoldCacheMutex(t *testing.T) {
	_, _, path := installedSourceFixture(t)
	if _, ok := Find("fixture-source", "sess"); !ok {
		t.Fatal("initial source missing")
	}
	sourceFixtureWrite(t, path, "waiting", 17)
	started, release := make(chan struct{}), make(chan struct{})
	sourceBeforeRPC = func(ctx context.Context) error {
		if !mu.TryLock() {
			return errors.New("source RPC started under cache mutex")
		}
		mu.Unlock()
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { defer close(stopped); finished <- PrepareSessionSources(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped; sourceBeforeRPC = nil })
	select {
	case <-started:
	case err := <-finished:
		t.Fatal("source preparation ended before entering RPC", err)
	case <-time.After(10 * time.Second):
		t.Fatal("source never entered RPC")
	}
	listed := make(chan []Session, 1)
	go func() { listed <- List(0) }()
	select {
	case sessions := <-listed:
		if !slices.ContainsFunc(sessions, func(session Session) bool { return session.Agent == "fixture-source" && session.Input == 7 }) {
			t.Fatal("last source snapshot was unavailable during concurrent preparation", sessions)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary List blocked behind source RPC")
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("canceled preparation published successfully")
	}
	close(release)
	sourceBeforeRPC = nil
	if err := PrepareSessionSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if session, ok := Find("fixture-source", "sess"); !ok || session.Input != 17 {
		t.Fatal("source did not recover after cancellation", session)
	}
}

func TestSourceNativeOpaqueAttachments(t *testing.T) {
	_, _, path := installedSourceFixture(t)
	blob := bytes.Repeat([]byte("opaque://not-an-image;data:do-not-decode\n"), 32768)
	digest := sha256.Sum256(blob)
	reference := fmt.Sprintf("%x", digest[:])
	blobPath := filepath.Join(filepath.Dir(path), "blob.bin")
	if err := os.WriteFile(blobPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	journal, _ := os.ReadFile(path)
	line, _ := json.Marshal(map[string]any{"blob": map[string]string{"path": "blob.bin", "sha256": reference}})
	if err := os.WriteFile(path, append(append(journal, line...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	session, ok := Find("fixture-source", "sess")
	if !ok || len(session.source.snapshot.files) != 2 {
		t.Fatal("ordinary native source did not acquire per-blob snapshots", session, SessionSourceErrors())
	}
	if _, err := TranscriptOf(session); err != nil {
		t.Fatal("native decoder did not receive opaque attachment", err)
	}
	previous := agentThere
	agentThere = func(agent string) bool { return agent == "omp" }
	t.Cleanup(func() { agentThere = previous })
	result, err := CarryOver(session, "omp")
	if err != nil {
		t.Fatal("ordinary source attachment conversion failed", err)
	}
	convertedBlob := filepath.Join(filepath.Dir(filepath.Dir(result.Path)), "zz-artifacts", "blob.bin")
	body, err := os.ReadFile(convertedBlob)
	if err != nil || !bytes.Equal(body, blob) {
		t.Fatal("opaque blob changed during checked ordinary conversion", err)
	}
	info, _ := os.Stat(blobPath)
	blob[0] ^= 1
	if err := os.WriteFile(blobPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(blobPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := TranscriptOf(session); err == nil {
		t.Fatal("same-size restored-mtime attachment mutation was accepted")
	}
	if _, err := CarryOver(session, "omp"); err == nil {
		t.Fatal("cached migration ignored changed source blob")
	}
}

func TestSourceNativeLargeDocument(t *testing.T) {
	_, _, path := installedSourceFixture(t)
	text := strings.Repeat("opaque document://string ", 131072)
	row, _ := json.Marshal(map[string]any{"id": "sess", "cwd": "/work/source", "title": "large",
		"role": "user", "text": text, "at": "2026-11-13T00:00:00Z", "input": 19})
	if err := os.WriteFile(path, append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	session, ok := Find("fixture-source", "sess")
	if !ok || session.Input != 19 || len(session.source.request.Documents) != 0 {
		t.Fatal("large ordinary source was not decoded/released", session, SessionSourceErrors())
	}
	transcript, err := TranscriptOf(session)
	if err != nil || !transcript.Cut || len(transcript.Parts) != 1 || transcript.Parts[0].Cut == 0 {
		t.Fatal("large native transcript ignored host output budgets", transcript.Cut, len(transcript.Parts), err)
	}
}
