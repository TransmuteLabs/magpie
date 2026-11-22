package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
)

func sourceForeignOff(t *testing.T, id string) {
	t.Helper()
	setSourceModuleInChild(t, "MAGPIE_SOURCE_TEST_HOST", "off", id)
}

func setSourceModuleInChild(t *testing.T, hostEnv, action, id string) {
	t.Helper()
	host := os.Getenv(hostEnv)
	if host == "" {
		t.Fatal("requires explicitly built synthetic Source host CLI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, host, "plugin", "sdk", action, id)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("foreign CLI %s failed: %v %s", action, err, output)
	}
}

func cycleSourceModule(t *testing.T, manager *extension.Manager, id, hostEnv string) {
	t.Helper()
	before, err := manager.ActiveModule(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	epoch := sourceEpoch.Load()
	catalog := extension.NewCatalog(manager.Namespace())
	for _, action := range []string{"off", "on"} {
		if hostEnv == "" {
			if err := catalog.SetEnabled(t.Context(), id, action == "on"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		setSourceModuleInChild(t, hostEnv, action, id)
	}
	after, err := manager.ActiveModule(t.Context(), id)
	if err != nil || after.Revision != before.Revision || after.EnableGeneration == before.EnableGeneration ||
		sourceEpoch.Load() != epoch {
		t.Fatal("foreign cycle did not change only the durable enablement generation", err)
	}
}

func TestSourceNativeForeignCycleRevokesSelectedBinding(t *testing.T) {
	manager, installed, _ := installedSourceFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".fixture-omp", "sessions", "bound.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFixtureWrite(t, path, "bound session", 7)
	if err := BindSessionSourceNative(t.Context(), installed.Manifest.ID, "native-omp", installed.Revision); err != nil {
		t.Fatal(err)
	}
	binding, err := os.ReadFile(sourceBindingsPath())
	if err != nil {
		t.Fatal(err)
	}
	selected, found := Find("omp", "bound")
	if !found || selected.source == nil {
		t.Fatal("ordinary Find did not use the bound JSONL source", selected)
	}
	assertTranscript := func(selected Session) {
		t.Helper()
		transcript, err := TranscriptOf(selected)
		if err != nil || len(transcript.Parts) != 1 || transcript.Parts[0].Text != "bound session" {
			t.Fatal("ordinary bound transcript unavailable", transcript, err)
		}
	}
	assertTranscript(selected)
	_, made, _ := ListUnless(0, -1)
	var publication sync.Once
	sourceBeforePublish = func() {
		publication.Do(func() {
			cycleSourceModule(t, manager, installed.Manifest.ID, "MAGPIE_SOURCE_TEST_HOST")
			if transcript, err := TranscriptOf(selected); !errors.Is(err, extension.ErrActionChanged) || len(transcript.Parts) != 0 {
				t.Error("selected JSONL transcript survived a foreign enablement cycle", transcript, err)
			}
		})
	}
	t.Cleanup(func() { sourceBeforePublish = nil })
	rows, _, same := ListUnless(0, made)
	sourceBeforePublish = nil
	if same || slices.ContainsFunc(rows, func(row Session) bool { return row.Agent == "omp" && row.ID == "bound" }) {
		t.Error("JSONL listing published a revoked generation", rows, same)
	}
	fresh, found := Find("omp", "bound")
	if !found || fresh.source == nil {
		t.Fatal("reenabled JSONL source did not honor the existing binding", fresh)
	}
	if _, err := TranscriptOf(selected); !errors.Is(err, extension.ErrActionChanged) {
		t.Error("fresh JSONL discovery revived the previous selection", err)
	}
	assertTranscript(fresh)
	if after, err := os.ReadFile(sourceBindingsPath()); err != nil || !bytes.Equal(binding, after) {
		t.Fatal("enablement cycle rewrote the persisted binding", err)
	}
}

func TestSourceNativeForeignOffDuringRead(t *testing.T) {
	_, installed, path := installedSourceFixture(t)
	before, found := Find("fixture-source", "sess")
	if !found {
		t.Fatal("initial native source missing")
	}
	marker := filepath.Join(t.TempDir(), "native-read-entered")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wait, err := json.Marshal(map[string]any{"wait": true, "text": marker})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(bytes.Clone(body), wait...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = PrepareSessionSources(ctx) }()
	stop := sync.OnceFunc(func() { cancel(); <-stopped })
	t.Cleanup(stop)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
waitForNative:
	for {
		if value, err := os.ReadFile(marker); err == nil && string(value) == "native-read-started" {
			break waitForNative
		}
		select {
		case <-stopped:
			t.Fatal("native decoder ended before barrier")
		case <-deadline.C:
			t.Fatal("actual native decoder did not reach barrier")
		case <-ticker.C:
		}
	}
	epoch := sourceEpoch.Load()
	sourceForeignOff(t, installed.Manifest.ID)
	if sourceEpoch.Load() != epoch {
		t.Fatal("foreign CLI unexpectedly notified local source epoch")
	}
	if slices.ContainsFunc(List(0), func(value Session) bool { return value.Agent == "fixture-source" }) {
		t.Error("ordinary List exposed a revoked source during active native read")
	}
	if _, found := Find("fixture-source", "sess"); found {
		t.Error("ordinary Find exposed a revoked source during active native read")
	}
	if _, err := TranscriptOf(before); err == nil {
		t.Error("revoked source retained transcript authority")
	}
}

func TestSourceNativeForeignOffBeforePublication(t *testing.T) {
	for _, operation := range []string{"list", "same", "find", "get", "titles", "agents", "manage", "list-agent", "native", "stats"} {
		t.Run(operation, func(t *testing.T) {
			_, installed, _ := installedSourceFixture(t)
			initial, made, _ := ListUnless(0, -1)
			if len(initial) == 0 {
				t.Fatal("initial source missing")
			}
			var once sync.Once
			sourceBeforePublish = func() {
				once.Do(func() {
					if !mu.TryLock() {
						t.Fatal("publication authority check holds cache mutex")
					}
					mu.Unlock()
					sourceForeignOff(t, installed.Manifest.ID)
				})
			}
			t.Cleanup(func() { sourceBeforePublish = nil })
			switch operation {
			case "list", "same":
				if operation == "list" {
					made = -1
				}
				out, _, same := ListUnless(0, made)
				if same || slices.ContainsFunc(out, func(value Session) bool { return value.Agent == "fixture-source" }) {
					t.Fatal("revoked source published as a row or cached-same response")
				}
			case "find":
				if _, found := Find("fixture-source", "sess"); found {
					t.Fatal("revoked source published by Find")
				}
			case "get":
				if _, found := Get("fixture-source:sess"); found {
					t.Fatal("revoked source published by Get")
				}
			case "titles":
				if Titles([]string{"fixture-source:sess"})["fixture-source:sess"] != "" {
					t.Fatal("revoked source title published")
				}
			case "agents":
				if slices.ContainsFunc(Agents(), func(value AgentCount) bool { return value.Agent == "fixture-source" }) {
					t.Fatal("revoked source agent count published")
				}
			case "manage":
				view := Manage(map[string]bool{"fixture-source": true}, func([]AgentCount, map[string]map[string]bool) string { return "fixture-source" })
				if len(view.Sessions) != 0 || len(view.Native["fixture-source"]) != 0 || len(view.Agents) != 0 {
					t.Fatal("revoked source management rows published", view)
				}
			case "list-agent":
				if len(ListAgent("fixture-source")) != 0 {
					t.Fatal("revoked source management listing published")
				}
			case "native":
				if len(NativeIDs(map[string]bool{"fixture-source": true})["fixture-source"]) != 0 {
					t.Fatal("revoked source native IDs published")
				}
			case "stats":
				if slices.ContainsFunc(StatsFor(0).Sessions, func(value Summary) bool { return value.Agent == "fixture-source" }) {
					t.Fatal("revoked source statistics published")
				}
			}
			sourceBeforePublish = nil
		})
	}
}
