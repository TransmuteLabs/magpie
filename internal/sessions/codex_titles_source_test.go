package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCodexTitlesUseSelectedHistorySource(t *testing.T) {
	_, native := carrySetup(t)
	if err := SetReads(nil, map[string]bool{"codex": true}); err != nil {
		t.Fatal(err)
	}
	const nativeName, delegatedName, nativeOnly = "Native index title", "Delegated fixture", "native-only"
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	index := fmt.Sprintf("{\"id\":%q,\"thread_name\":%q,\"updated_at\":%q}\n"+
		"{\"id\":%q,\"thread_name\":\"Other native title\"}\n", carryCodex, nativeName, stamp, nativeOnly)
	writeHistoryAuthority(t, codexTitleIndexPath(), []byte(index))
	ids := []string{carryCodex, nativeOnly}
	nativeTitles := map[string]string{carryCodex: nativeName, nativeOnly: "Other native title"}
	if got := CodexTitles(ids); !maps.Equal(got, nativeTitles) {
		t.Fatalf("native title fixture: %v", got)
	}
	digests := []string{CodexTitleDigest(nativeName)}
	if got := CodexTitleRecipients(digests); len(got[carryCodex]) != 1 {
		t.Fatalf("native title application fixture: %v", got)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	const sourceRoot = ".fixture/codex"
	journal, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	writeHistoryAuthority(t, filepath.Join(home, sourceRoot, carryCodex+".jsonl"), journal)
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
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.codex-titles", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 6}, Capabilities: []sdk.Capability{sdk.SessionSource}},
		SessionSources: []sdk.SessionSourceDescriptor{{ID: "history", Agent: "codex", Codec: "fixture-jsonl",
			Container: "jsonl", Resume: "none",
			Stores: []sdk.SessionSourceStore{{Slot: "selected", HomeRelativePath: sourceRoot}},
			Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}}}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeHistoryAuthority(t, filepath.Join(directory, sdk.ManifestFile), body)
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
	if err := BindSessionSourceNative(t.Context(), manifest.ID, "history", installed.Revision); err != nil {
		t.Fatal(err)
	}
	bindings, err := readSourceBindings()
	if err != nil || len(bindings) != 1 {
		t.Fatalf("fixture binding: %v %v", bindings, err)
	}

	if got := CodexTitles(ids); !maps.Equal(got, map[string]string{carryCodex: delegatedName}) {
		t.Errorf("CodexTitles ignored selected summary: %v", got)
	}
	key := "codex:" + carryCodex
	if got := Titles([]string{key}); got[key] != delegatedName {
		t.Errorf("Titles replaced selected summary: %v", got)
	}
	if session, found := Find("codex", carryCodex); !found || session.source == nil || session.Title != delegatedName {
		t.Errorf("Find lost selected summary: found=%v source=%v title=%q problems=%v", found, session.source != nil, session.Title, SessionSourceErrors())
	}
	if got := CodexTitleRecipients(digests); len(got) != 0 {
		t.Errorf("native applications survived delegation: %v", got)
	}

	if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := CodexTitles(ids); len(got) != 0 {
		t.Errorf("closed source published titles: %v", got)
	}
	if err := UnbindSessionSourceNative(t.Context(), "codex", bindings[0].Identity); err != nil {
		t.Fatal(err)
	}
	if got := CodexTitles(ids); !maps.Equal(got, nativeTitles) {
		t.Errorf("unbinding did not restore native titles: %v", got)
	}
	writeHistoryAuthority(t, sourceBindingsPath(), []byte("{"))
	if got := CodexTitles(ids); len(got) != 0 {
		t.Errorf("invalid authority published titles: %v", got)
	}
	if got := CodexTitleRecipients(digests); len(got) != 0 {
		t.Errorf("invalid authority published native applications: %v", got)
	}
}
