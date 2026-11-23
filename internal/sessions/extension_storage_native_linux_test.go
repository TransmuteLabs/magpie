package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
)

type storageWriterState struct {
	PID      int    `json:"pid"`
	MainOnly int    `json:"mainOnly"`
	MediaID  string `json:"mediaId"`
	DB       string `json:"db"`
	WAL      string `json:"wal"`
	SHM      string `json:"shm"`
	Busy     int    `json:"busy"`
}

type storageNativeFixture struct {
	manager    *extension.Manager
	entry      extension.Installed
	writer     *json.Encoder
	reader     *json.Decoder
	stderrPath string
	initial    storageWriterState
	consent    StorageSourceConsent
	root       StorageSourceRoot
	directory  string
}

func installStorageNativeFixture(t testing.TB) *storageNativeFixture {
	t.Helper()
	return startStorageNativeFixture(t, 0)
}

func startStorageNativeFixture(t testing.TB, rows int) *storageNativeFixture {
	t.Helper()
	distribution := os.Getenv("MAGPIE_STORAGE_TEST_DIST")
	script := os.Getenv("MAGPIE_STORAGE_TEST_WRITER")
	if distribution == "" || script == "" {
		t.Skip("requires the explicitly built OpenCode native storage package and synthetic SQLite writer")
	}
	consentHome(t)
	if err := SetReads(nil, map[string]bool{"opencode": true}); err != nil {
		t.Fatal(err)
	}
	directory := OpenCodeDir()
	args := []string{script, directory}
	if rows > 0 {
		args = []string{"-c", scaleStorageWriter, script, directory, strconv.Itoa(rows)}
	}
	command := proc.CommandContext(t.Context(), "python3", args...)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "writer-stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &storageNativeFixture{writer: json.NewEncoder(input),
		reader: json.NewDecoder(bufio.NewReader(output)), stderrPath: stderr.Name(), directory: directory}
	t.Cleanup(func() {
		_ = input.Close()
		if err := command.Wait(); err != nil && t.Context().Err() == nil {
			t.Errorf("owned SQLite writer failed: %v %s", err, fixture.stderrText())
		}
	})
	fixture.initial = fixture.next(t)
	if fixture.initial.PID == os.Getpid() || fixture.initial.PID <= 0 || fixture.initial.MainOnly != 0 {
		t.Fatal("fixture did not establish a separate writer and a WAL-only committed session", fixture.initial)
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
		t.Fatal("ordinary native storage package installation failed", err)
	}
	fixture.manager, fixture.entry = manager, entry
	roots, consents, err := InspectStorageSources(t.Context())
	if err != nil || len(roots) != 1 || len(consents) != 0 || roots[0].Consented {
		t.Fatal("ordinary storage inspection did not require root consent", roots, consents, err)
	}
	fixture.root = roots[0]
	if err := BindStorageSource(t.Context(), entry.Manifest.ID, roots[0].Source, roots[0].RootSlot, "foreign", entry.Revision); err == nil {
		t.Fatal("root consent accepted an uninspected root reference")
	}
	if err := BindStorageSource(t.Context(), entry.Manifest.ID, roots[0].Source, roots[0].RootSlot, roots[0].RootRef, entry.Revision); err != nil {
		t.Fatal(err)
	}
	_, consents, err = InspectStorageSources(t.Context())
	if err != nil || len(consents) != 1 {
		t.Fatal("checked root consent was not persisted", consents, err)
	}
	fixture.consent = consents[0]
	return fixture
}

func (fixture *storageNativeFixture) stderrText() string {
	body, err := os.ReadFile(fixture.stderrPath)
	if err != nil {
		return err.Error()
	}
	return string(body)
}

func (fixture *storageNativeFixture) next(t testing.TB) storageWriterState {
	t.Helper()
	var state storageWriterState
	if err := fixture.reader.Decode(&state); err != nil {
		t.Fatal("owned SQLite writer did not reply", err, fixture.stderrText())
	}
	return state
}

func (fixture *storageNativeFixture) ask(t testing.TB, command string) storageWriterState {
	t.Helper()
	if err := fixture.writer.Encode(command); err != nil {
		t.Fatal(err)
	}
	return fixture.next(t)
}

func storageSelected(t *testing.T) Session {
	t.Helper()
	selected, found := Find("opencode", "ses_wal")
	if !found || selected.source == nil || selected.source.storage == nil {
		t.Fatal("ordinary Find did not select the installed native SQLite source", selected)
	}
	return selected
}

func TestStorageNativeForeignCycleRevokesSelectedConsent(t *testing.T) {
	fixture := installStorageNativeFixture(t)
	consent, err := os.ReadFile(storageConsentsPath())
	if err != nil {
		t.Fatal(err)
	}
	selected := storageSelected(t)
	assertTranscript := func(selected Session) {
		t.Helper()
		transcript, err := TranscriptOf(selected)
		if err != nil || !slices.ContainsFunc(transcript.Parts, func(part Part) bool {
			return part.Text == "first split part"
		}) {
			t.Fatal("ordinary SQLite transcript unavailable", transcript, err)
		}
	}
	assertTranscript(selected)
	sourceBeforePublish = func() {
		cycleSourceModule(t, fixture.manager, fixture.entry.Manifest.ID, "MAGPIE_STORAGE_TEST_HOST")
	}
	t.Cleanup(func() { sourceBeforePublish = nil })
	transcript, err := TranscriptOf(selected)
	sourceBeforePublish = nil
	if !errors.Is(err, extension.ErrActionChanged) || len(transcript.Parts) != 0 {
		t.Error("SQLite transcript published after a foreign enablement cycle", transcript, err)
	}
	fresh := storageSelected(t)
	if _, err := TranscriptOf(selected); !errors.Is(err, extension.ErrActionChanged) {
		t.Error("fresh SQLite discovery revived the previous selection", err)
	}
	assertTranscript(fresh)
	if after, err := os.ReadFile(storageConsentsPath()); err != nil || !bytes.Equal(consent, after) {
		t.Fatal("enablement cycle rewrote the persisted consent", err)
	}
}

func TestStorageNativeOrdinarySQLiteSource(t *testing.T) {
	fixture := installStorageNativeFixture(t)
	selected := storageSelected(t)
	if selected.Title != "Committed WAL session" || selected.Input != 7 || selected.Output != 3 ||
		selected.CacheRead != 2 || selected.CacheWrite != 1 || !strings.HasPrefix(selected.Path, "storage:") {
		t.Fatal("ordinary normalized source metadata lost WAL or token semantics", selected)
	}
	listed := ListAgent("opencode")
	if len(listed) != 2 || slices.ContainsFunc(listed, func(value Managed) bool { return value.ID == "ses_leftover" }) {
		t.Fatal("V2/V1/database precedence did not deduplicate ordinary source rows", listed)
	}
	if _, found := Get("opencode:ses_wal"); !found {
		t.Fatal("ordinary selected source key was unavailable")
	}
	transcript, err := TranscriptOf(selected)
	if err != nil {
		t.Fatal("unchanged ordinary refresh discarded the selected SQLite session", err)
	}
	encoded, _ := json.Marshal(transcript)
	body := string(encoded)
	first, second, inline := strings.Index(body, "first split part"), strings.Index(body, "second split part"), strings.Index(body, "inline tail")
	if first < 0 || second <= first || inline <= second || strings.Contains(body, "retired instruction") ||
		!strings.Contains(body, "current instruction") || !strings.Contains(body, "media unavailable: external") {
		t.Fatal("ordinary transcript lost ordered parts, epoch or explicit unavailable media", body)
	}
	hit, found, err := SearchSession(t.Context(), selected, NeedleOf("first split part"))
	if err != nil || !found || hit.Match == "" {
		t.Fatal("ordinary source search did not read the selected transaction", hit, found, err)
	}
	media, err := StorageSourceMedia(t.Context(), "opencode", selected.ID, fixture.initial.MediaID)
	if err != nil || media.MIME != "image/png" || len(media.Bytes) == 0 {
		t.Fatal("ordinary selected media route lost the deduplicated blob", media, err)
	}
	if _, err := StorageSourceMedia(t.Context(), "opencode", "ses_v1", fixture.initial.MediaID); err == nil {
		t.Fatal("another logical session borrowed the selected blob")
	}
	mu.Lock()
	for path := range savable() {
		if strings.HasPrefix(path, "storage:") {
			mu.Unlock()
			t.Fatal("logical SQLite session was persisted as a physical parse")
		}
	}
	mu.Unlock()
	after := fixture.ask(t, "state")
	if after.DB != fixture.initial.DB || after.WAL != fixture.initial.WAL || after.SHM != fixture.initial.SHM {
		t.Fatal("read-only ordinary source changed donor DB/WAL/SHM bytes")
	}
	for range 40 {
		if err := PrepareSessionSources(t.Context()); err != nil {
			t.Fatal("completed ordinary catalogs exhausted pending snapshot capacity", err)
		}
	}
	fixture.ask(t, "uncommitted")
	uncommitted, err := TranscriptOf(storageSelected(t))
	encoded, _ = json.Marshal(uncommitted)
	if err != nil || bytes.Contains(encoded, []byte("uncommitted secret")) {
		t.Fatal("ordinary read included the separate writer's uncommitted message", err, string(encoded))
	}
	fixture.ask(t, "rollback")
	fixture.ask(t, "append")
	changed := storageSelected(t)
	if changed.Title != "Updated WAL session" {
		t.Fatal("ordinary refresh did not see a separate WAL writer commit", changed)
	}
	if _, err := TranscriptOf(selected); err == nil {
		t.Fatal("changed ordinary source retained the superseded summary's authority")
	}
	updated, err := TranscriptOf(changed)
	encoded, _ = json.Marshal(updated)
	if err != nil || !bytes.Contains(encoded, []byte("later committed message")) {
		t.Fatal("fresh transaction omitted committed WAL content", err, string(encoded))
	}
	if state := fixture.ask(t, "checkpoint"); state.Busy != 0 {
		t.Fatal("ordinary read leaked WAL checkpoint locks", state)
	}
	if _, err := TranscriptOf(storageSelected(t)); err != nil {
		t.Fatal("ordinary reader could not reopen after a separate process reset WAL", err)
	}
	fixture.ask(t, "migrate")
	if _, found := Find("opencode", "ses_v1"); found {
		t.Fatal("ordinary source republished leftover V1 rows after completed migration")
	}
}

func TestStorageNativePublicationRechecksOrdinaryOwners(t *testing.T) {
	for _, operation := range []string{"reads", "consent", "config", "off", "root", "cancel", "foreign-off"} {
		t.Run(operation, func(t *testing.T) {
			fixture := installStorageNativeFixture(t)
			selected := storageSelected(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var once sync.Once
			sourceBeforePublish = func() {
				once.Do(func() {
					switch operation {
					case "reads":
						if err := SetReads(nil, map[string]bool{"opencode": false}); err != nil {
							t.Fatal(err)
						}
					case "consent":
						if err := UnbindStorageSource(t.Context(), "opencode", fixture.consent.Identity); err != nil {
							t.Fatal(err)
						}
					case "config":
						config, err := fixture.manager.ModuleConfig(t.Context(), fixture.entry.Manifest.ID)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := fixture.manager.CompareAndSwapModuleConfig(t.Context(), fixture.entry.Manifest.ID, config.Version,
							json.RawMessage(`{"projectId":"changed"}`)); err != nil {
							t.Fatal(err)
						}
					case "off":
						if err := fixture.manager.SetEnabled(t.Context(), fixture.entry.Manifest.ID, false); err != nil {
							t.Fatal(err)
						}
					case "root":
						if err := os.Rename(fixture.directory, fixture.directory+".replaced"); err != nil {
							t.Fatal(err)
						}
						if err := os.MkdirAll(fixture.directory, 0o700); err != nil {
							t.Fatal(err)
						}
					case "cancel":
						cancel()
					case "foreign-off":
						binary := os.Getenv("MAGPIE_STORAGE_TEST_HOST")
						if binary == "" {
							t.Fatal("ordinary foreign revocation needs the explicitly built host")
						}
						command := proc.CommandContext(t.Context(), binary, "plugin", "sdk", "off", fixture.entry.Manifest.ID)
						command.Env = os.Environ()
						if output, err := command.CombinedOutput(); err != nil {
							t.Fatalf("foreign owner revocation failed: %v %s", err, output)
						}
					}
				})
			}
			t.Cleanup(func() { sourceBeforePublish = nil })
			if _, err := storageSourceRead(ctx, selected.source); err == nil {
				t.Fatal("ordinary source published after its owner changed", operation)
			}
			sourceBeforePublish = nil
		})
	}
}

func TestStorageNativeRootedAccessRejectsForeignRawMembers(t *testing.T) {
	fixture := installStorageNativeFixture(t)
	selected := storageSelected(t)
	if _, err := StorageSourceMedia(t.Context(), "opencode", selected.ID, "../main-only.db"); err == nil {
		t.Fatal("ordinary media accepted a raw relative file selector")
	}
	if err := os.Rename(filepath.Join(fixture.directory, "opencode.db-wal"), filepath.Join(fixture.directory, "saved-wal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.directory, "saved-wal"), filepath.Join(fixture.directory, "opencode.db-wal")); err != nil {
		t.Fatal(err)
	}
	if _, err := storageSourceRead(t.Context(), selected.source); err == nil || err == io.EOF {
		t.Fatal("ordinary native reader accepted a symlink sidecar", err)
	}
}
