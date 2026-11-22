package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSourcePublicationRechecksForeignEnable(t *testing.T) {
	const agent = "fixture-source"
	consentHome(t)
	if err := SetReads(nil, map[string]bool{agent: true}); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	distribution := t.TempDir()
	binary := "worker"
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-p=1", "-o", filepath.Join(distribution, binary), "../extension/testdata/actionworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build source fixture: %v\n%s", err, output)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	install := func(id, directory, session string) string {
		t.Helper()
		manifest := sdk.Manifest{Schema: 1, ID: id, Version: "1.0.0", Runtime: "exec", Entry: binary,
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 6}, Capabilities: []sdk.Capability{sdk.SessionSource}},
			SessionSources: []sdk.SessionSourceDescriptor{{ID: "history", Agent: agent, Codec: "fixture-jsonl",
				Container: "jsonl", Transcript: true, Resume: "none",
				Stores: []sdk.SessionSourceStore{{Slot: "main", HomeRelativePath: directory}},
				Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}}}}
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		writeHistoryAuthority(t, filepath.Join(distribution, sdk.ManifestFile), body)
		if _, err := manager.Install(t.Context(), distribution); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(home, directory, session+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		sourceFixtureWrite(t, path, session, 7)
		return path
	}
	const primary, competitor = "fixture.source-primary", "fixture.source-competitor"
	primaryPath := install(primary, ".fixture-source/primary", "primary")
	install(competitor, ".fixture-source/competitor", "competitor")
	catalog := extension.NewCatalog(manager.Namespace())
	if err := catalog.SetEnabled(t.Context(), competitor, false); err != nil {
		t.Fatal(err)
	}
	first, found := Find(agent, "primary")
	if !found || first.source == nil {
		t.Fatalf("ordinary discovery did not admit the primary source: %v", SessionSourceErrors())
	}
	if _, err := selectSessionHistoryOwner(t.Context(), first); err != nil {
		t.Fatalf("selected source fixture has no read authority: %v", err)
	}
	sourceFixtureWrite(t, filepath.Join(filepath.Dir(primaryPath), "next.jsonl"), "new session", 11)
	epoch := sourceEpoch.Load()
	enabled := false
	sourceBeforeRPC = func(ctx context.Context) error {
		if enabled {
			return nil
		}
		if err := catalog.SetEnabled(ctx, competitor, true); err != nil {
			return err
		}
		enabled = true
		if sourceEpoch.Load() != epoch {
			t.Error("the separate catalog writer signaled the local epoch")
		}
		if _, err := TranscriptOf(first); !errors.Is(err, extension.ErrActionChanged) {
			t.Errorf("held transcript was not rejected at the authority check: %v", err)
		}
		if _, found := Find(agent, "primary"); found {
			t.Error("Find exposed the held source during ambiguous preparation")
		}
		return nil
	}
	t.Cleanup(func() { sourceBeforeRPC = nil })
	err = PrepareSessionSources(t.Context())
	sourceBeforeRPC = nil
	if !enabled || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("preparation under the old selection returned %v, competitor enabled: %v", err, enabled)
	}
	if files := extensionSourceFiles(nil); len(files) != 0 {
		t.Errorf("preparation published %d records without a unique owner", len(files))
	}
	if err := catalog.SetEnabled(t.Context(), primary, false); err != nil {
		t.Fatal(err)
	}
	if second, found := Find(agent, "competitor"); !found || second.source == nil {
		t.Fatal("single competing source did not become discoverable after the primary was disabled")
	}
}
