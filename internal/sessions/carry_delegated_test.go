package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCarryUsesDelegatedSourceRoutes(t *testing.T) {
	carrySetup(t)
	if err := SetReads(nil, map[string]bool{"claude": true, "pi": true}); err != nil {
		t.Fatal(err)
	}
	oldOMP := ompThere
	ompThere = func() bool { return true }
	t.Cleanup(func() { ompThere = oldOMP })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-p=1", "-o", filepath.Join(directory, entry), "../extension/testdata/actionworker")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build action fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.delegated-carry", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 6}, Capabilities: []sdk.Capability{sdk.SessionSource, sdk.SessionMigration}}}
	pairs := []struct{ from, to, dir string }{
		{"claude", "codex", ".fixture/converted-codex"},
		{"pi", "omp", ".fixture/converted-omp"},
	}
	journal := []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"selected fixture\"}}\n")
	for _, pair := range pairs {
		root := ".fixture/" + pair.from
		manifest.SessionSources = append(manifest.SessionSources, sdk.SessionSourceDescriptor{
			ID: pair.from, Agent: pair.from, Codec: "fixture-jsonl", Container: "jsonl", Resume: "none",
			Stores: []sdk.SessionSourceStore{{Slot: "selected", HomeRelativePath: root}},
			Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"},
		})
		manifest.Sessions = append(manifest.Sessions, sdk.SessionDescriptor{
			ID: pair.from + "-converter", Name: "Selected history", From: pair.from, To: pair.to, OutputDir: pair.dir,
		})
		source := filepath.Join(home, root, "selected.jsonl")
		if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, journal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	installed, err := manager.Install(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("native", func(t *testing.T) {
		session, found := Find("claude", carryClaude)
		if !found || session.source != nil {
			t.Fatalf("missing native Claude history: %+v", session)
		}
		want := []Carry{{Agent: "codex", Convert: true}}
		if !slices.Equal(session.Carry, want) {
			t.Fatalf("expected one Codex conversion: %+v", session.Carry)
		}
		wantPath := filepath.Join(home, pairs[0].dir, "converted-session.jsonl")
		if _, err := os.Stat(wantPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("listing published an installed conversion: %v", err)
		}
		result, err := CarryOver(session, "codex")
		if err != nil || result.ID != "converted-session" || result.Path != wantPath {
			t.Fatalf("expected the installed converter for native history: %+v %v", result, err)
		}
		var converted struct {
			Source []sdk.SessionDocument `json:"source"`
		}
		if body := sum(t, wantPath); json.Unmarshal([]byte(body), &converted) != nil ||
			len(converted.Source) != 1 || string(converted.Source[0].Body) != sum(t, session.Path) {
			t.Fatalf("installed converter did not receive the selected native history: %s", body)
		}
		if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
			t.Fatal(err)
		}
		session, found = Find("claude", carryClaude)
		if !found || !slices.Equal(session.Carry, want) {
			t.Fatalf("linked converter was not offered after disabling the module: %+v", session.Carry)
		}
		result, err = CarryOver(session, "codex")
		if err != nil || result.ID == "" || result.ID == "converted-session" {
			t.Fatalf("expected the linked converter after disabling the module: %+v %v", result, err)
		}
		if rollout := readLines(t, result.Path); len(rollout) == 0 || rollout[0]["type"] != "session_meta" {
			t.Fatalf("linked converter did not publish a Codex rollout: %+v", result)
		}
		if err := os.Remove(wantPath); err != nil {
			t.Fatal(err)
		}
		if err := manager.SetEnabled(t.Context(), manifest.ID, true); err != nil {
			t.Fatal(err)
		}
	})
	for _, pair := range pairs {
		if err := BindSessionSourceNative(t.Context(), manifest.ID, pair.from, installed.Revision); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range pairs {
		t.Run(pair.from+"-to-"+pair.to, func(t *testing.T) {
			session, found := Find(pair.from, "selected")
			if !found || session.source == nil || session.Transcript {
				t.Fatalf("expected ordinary delegated JSONL selection without transcript: %+v (%v)", session, SessionSourceErrors())
			}
			want := []Carry{{Agent: pair.to, Convert: true}}
			if !slices.Equal(session.Carry, want) {
				t.Fatalf("delegated carry selection = %+v, want %+v", session.Carry, want)
			}
			result, err := CarryOver(session, pair.to)
			wantPath := filepath.Join(home, pair.dir, "converted-session.jsonl")
			if err != nil || result.ID != "converted-session" || result.Path != wantPath {
				t.Fatalf("ordinary CarryOver did not invoke the installed route: %+v %v", result, err)
			}
			output, err := os.ReadFile(result.Path)
			var converted struct {
				Source []sdk.SessionDocument `json:"source"`
			}
			if err != nil || json.Unmarshal(output, &converted) != nil || len(converted.Source) != 1 ||
				!bytes.Equal(converted.Source[0].Body, journal) {
				t.Fatalf("selected journal did not reach the installed converter: %s %v", output, err)
			}
			if pair.from == "claude" {
				owner, err := selectSessionHistoryOwner(t.Context(), session)
				if err != nil {
					t.Fatal(err)
				}
				result, err := convertLinkedSession(t.Context(), owner, linkedSessionConverters[0])
				if !errors.Is(err, extension.ErrActionChanged) || result != (CarryResult{}) {
					t.Fatalf("linked converter accepted delegated owner: %+v %v", result, err)
				}
			}
		})
		t.Run(pair.from+"-opaque", func(t *testing.T) {
			session := Session{Agent: pair.from, ID: "selected", Path: "storage:root/selected", Cwd: "/work",
				source: &sessionSourceRecord{storage: &storageSourceRecord{}}}
			if got := carries(session); len(got) != 0 {
				t.Fatalf("opaque storage advertised a raw journal route: %+v", got)
			}
		})
	}
}
