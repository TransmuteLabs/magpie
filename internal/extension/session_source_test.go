package extension

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestBunSessionSourceThroughGoHost(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun runtime is not installed")
	}
	t.Setenv("MAGPIE_BUN", bun)
	directory := bunFixturePackage(t)
	name := "test/fixtures/session-source-worker.js"
	body, err := os.ReadFile(filepath.Join("..", "..", "sdk", "js", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(name)), body, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "source.conformance", Version: "1.0.0", Runtime: "bun", Entry: name,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 6}, Capabilities: []sdk.Capability{sdk.SessionSource}},
		SessionSources: []sdk.SessionSourceDescriptor{{ID: "history", Agent: "fixture-source", Codec: "fixture-jsonl", Container: "jsonl",
			Stores: []sdk.SessionSourceStore{{Slot: "main", HomeRelativePath: ".fixture/sessions"}},
			Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}, Transcript: true, Resume: "none"}}}
	body, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	request := sdk.SessionSourceReadRequest{SourceID: "history",
		Locator:  sdk.SessionSourceLocator{StoreRef: "store", Path: "sess.jsonl", SessionID: "sess"},
		Snapshot: strings.Repeat("a", 64), Mode: "summary",
		Documents: []sdk.SessionDocument{{Path: "sess.jsonl", Format: "fixture-jsonl", Body: []byte(`{"text":"Go to Bun Source 1.6"}`)}},
		Limits:    sdk.SessionSourceLimits{MaxParts: 10, MaxTextBytes: 1000}}
	lease, err := manager.Acquire(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := lease.Call(t.Context(), sdk.MethodSessionSourceRead, sdk.Scope{Session: "other"}, request, nil); err == nil {
		t.Fatal("source lease accepted another session scope")
	}
	raw, err := lease.Call(t.Context(), sdk.MethodSessionSourceRead, sdk.Scope{Session: "sess"}, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := sdk.DecodeSessionSourceResult(request, raw)
	if err != nil || result.Summary.Title != "Go to Bun Source 1.6" || result.Summary.Usage == nil {
		t.Fatal("actual Go/Bun source read failed", result, err)
	}
	request.Mode = "transcript"
	raw, err = lease.Call(t.Context(), sdk.MethodSessionSourceRead, sdk.Scope{Session: "sess"}, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err = sdk.DecodeSessionSourceResult(request, raw)
	if err != nil || len(result.Parts) != 1 || result.Parts[0].Text != "Go to Bun Source 1.6" {
		t.Fatal("actual Go/Bun source transcript failed", result, err)
	}
}
