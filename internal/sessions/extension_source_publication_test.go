package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func sourceForeignOff(t *testing.T, id string) {
	t.Helper()
	host := os.Getenv("MAGPIE_SOURCE_TEST_HOST")
	if host == "" {
		t.Fatal("requires explicitly built synthetic Source host CLI")
	}
	command := exec.CommandContext(t.Context(), host, "plugin", "sdk", "off", id)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("foreign CLI off failed: %v %s", err, output)
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
