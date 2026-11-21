package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/sessions"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestQueryPageKeepsCallsUnderBoundJSONLHistory(t *testing.T) {
	pageHome(t)
	if err := sessions.SetReads(nil, map[string]bool{"codex": true}); err != nil {
		t.Fatal(err)
	}
	now := holdClock(t, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	const thread = "019a1111-2222-7333-8444-555566667777"
	path := filepath.Join(sessions.CodexDir(), "sessions", "rollout-2026-11-12T11-00-00-"+thread+".jsonl")
	writeCodexCall(t, path, thread, thread, `"cli"`, "native-response", "native-model", "high", now.Add(-time.Minute), 40, 4)
	Append(Record{Time: now, Agent: "codex", Provider: "openai", Model: "gateway-model", Input: 10, Output: 2,
		Status: 200, Session: "gateway-session", ResponseID: "gateway-response"})

	dir, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-p=1", "-o", filepath.Join(dir, entry), "../extension/testdata/actionworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build action fixture: %v\n%s", err, output)
	}
	write := func(path string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const sourceRoot = ".fixture/codex"
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(home, sourceRoot, thread+".jsonl")
	write(sourcePath, []byte("{}\n"))
	sourcePath, err = filepath.EvalSymlinks(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.codex-accounting", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 6}, Capabilities: []sdk.Capability{sdk.SessionSource}},
		SessionSources: []sdk.SessionSourceDescriptor{{ID: "history", Agent: "codex", Codec: "fixture-jsonl",
			Container: "jsonl", Resume: "none",
			Stores: []sdk.SessionSourceStore{{Slot: "selected", HomeRelativePath: sourceRoot}},
			Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}}}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, sdk.ManifestFile), body)
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	installed, err := manager.Install(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, wantLocal bool) {
		t.Helper()
		for _, calls := range [][]sessions.Call{sessions.Calls(time.Time{}), sessions.AgentCalls(time.Time{}, "codex")} {
			got := slices.ContainsFunc(calls, func(call sessions.Call) bool {
				return call.ResponseID == "native-response" && call.Input == 40 && call.Output == 4
			})
			if got != wantLocal {
				t.Errorf("Calls kept measured fixture = %v, want %v: %+v", got, wantLocal, calls)
			}
		}
		page := QueryPage(All, Filter{}, 0, 100)
		has := func(id string, input, output int) bool {
			return slices.ContainsFunc(page.Rows, func(row Row) bool {
				return row.ResponseID == id && row.Input == input && row.Output == output
			})
		}
		wantRows := 1
		if wantLocal {
			wantRows++
		}
		if page.Total != wantRows || !has("gateway-response", 10, 2) || has("native-response", 40, 4) != wantLocal {
			t.Errorf("Usage total = %d, want %d distinct measured rows: %+v", page.Total, wantRows, page.Rows)
		}
	}
	t.Run("native", func(t *testing.T) {
		check(t, true)
		if identities := sessions.CodexIdentities(); identities[thread].ID != thread {
			t.Fatal("native thread identity fixture missing", identities)
		}
	})
	if err := sessions.BindSessionSourceNative(t.Context(), manifest.ID, "history", installed.Revision); err != nil {
		t.Fatal(err)
	}
	bindings := filepath.Join(appdir.Config(), "session-source-bindings.json")
	bound, err := os.ReadFile(bindings)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("cold", func(t *testing.T) {
		sessions.Reset()
		check(t, true)
	})
	t.Run("warm", func(t *testing.T) {
		if selected, found := sessions.Find("codex", thread); !found || selected.Path != sourcePath {
			t.Fatalf("SDK journal did not own history: %+v, found=%v, errors=%v", selected, found, sessions.SessionSourceErrors())
		}
		check(t, true)
		if sessions.CodexIdentities()[thread].ID != "" || sessions.CodexThreadCwd(thread) != "" {
			t.Error("accounting restored native thread metadata")
		}
	})
	t.Run("read-denied", func(t *testing.T) {
		if err := sessions.SetReads(nil, map[string]bool{"codex": false}); err != nil {
			t.Fatal(err)
		}
		check(t, false)
		if err := sessions.SetReads(nil, map[string]bool{"codex": true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rooted-claim", func(t *testing.T) {
		before := sessions.InspectCallHistory()
		consents := filepath.Join(appdir.Config(), "storage-source-consents.json")
		body, err := json.Marshal([]sessions.StorageSourceConsent{{Plugin: "storage-owner", Agent: "codex",
			Source: "storage", RootSlot: "data", RootRef: strings.Repeat("a", 64), Identity: strings.Repeat("b", 64)}})
		if err != nil {
			t.Fatal(err)
		}
		write(consents, body)
		check(t, false)
		if before.Current() {
			t.Error("active rooted claim did not revoke the accounting snapshot")
		}
		if err := os.Remove(consents); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("malformed-claim", func(t *testing.T) {
		write(bindings, []byte("{invalid"))
		check(t, false)
		write(bindings, bound)
	})
	t.Run("disabled-owner", func(t *testing.T) {
		if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
			t.Fatal(err)
		}
		check(t, false)
	})
}
