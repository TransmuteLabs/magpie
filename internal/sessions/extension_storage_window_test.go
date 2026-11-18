package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func windowStorageFixture(t *testing.T) (*extension.Manager, WindowStorageOwner) {
	t.Helper()
	consentHome(t)
	root := t.TempDir()
	manager := extension.NewManager(root)
	old := sourceManager
	sourceManager = func() *extension.Manager { return manager }
	t.Cleanup(func() { sourceManager = old; manager.Close(context.Background()); windowStorageBeforeReadGate = nil })
	manifest := sdk.Manifest{Schema: 1, ID: "fixture-window", Version: "1.0.0", Runtime: "native", Entry: "module.so",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.StorageSource}},
		StorageSources: []sdk.StorageSourceDescriptor{{ID: "history", Agent: "fixture", Codec: "fixture-db",
			Roots: []sdk.SessionSourceStore{{Slot: "first", HomeRelativePath: ".window-history/first"},
				{Slot: "second", HomeRelativePath: ".window-history/second"}},
			Families: []sdk.StorageFamily{{ID: "db", Primary: "fixture.db"}}, Transcript: true, Resume: "none"}}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for name, data := range map[string][]byte{sdk.ManifestFile: body, "module.so": []byte("inspection must never load this invalid native file")} {
		if err := os.WriteFile(filepath.Join(source, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := extension.NewCatalog(root).Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	for _, slot := range []string{"first", "second"} {
		if err := os.MkdirAll(filepath.Join(home, ".window-history", slot), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config, err := manager.ModuleConfig(t.Context(), entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return manager, WindowStorageOwner{Module: entry.Manifest.ID, PackageRevision: entry.Revision,
		ConfigVersion: config.Version, PageRef: "fixture-page", Component: "window:page"}
}

func TestWindowStorageMetadataIsNotConsent(t *testing.T) {
	_, owner := windowStorageFixture(t)
	before, err := settings.InspectSessionReads()
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil || inspection.ReadAllowed || inspection.OwnOff || len(inspection.Roots) != 2 {
		t.Fatal("metadata discovery failed or granted read consent", inspection, err)
	}
	for _, root := range inspection.Roots {
		if !root.Available || root.Consented || root.ConsentRef != "" {
			t.Fatal("root metadata became authority", root)
		}
		if _, err := ListWindowStorage(t.Context(), owner, "history", inspection.ViewRef, root.RootRef, "", 1); !errors.Is(err, ErrNotRead) {
			t.Fatal("metadata opened the native catalog", err)
		}
	}
	after, err := settings.InspectSessionReads()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("inspection wrote ordinary consent", before, after, err)
	}
	if _, err := os.Stat(storageConsentsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection wrote root consent", err)
	}
	if _, err := InspectWindowStorage(t.Context(), owner, "foreign"); err == nil {
		t.Fatal("foreign source inspected")
	}
	if _, err := ReadWindowStorage(t.Context(), owner, "history", inspection.ViewRef, strings.Repeat("a", 64)); err == nil {
		t.Fatal("unissued session reference was accepted")
	}
}

func TestWindowStorageExactReleaseAndConsentABA(t *testing.T) {
	_, owner := windowStorageFixture(t)
	if err := SetReads(nil, map[string]bool{"codex": false, "claude": true}); err != nil {
		t.Fatal(err)
	}
	initial, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil {
		t.Fatal(err)
	}
	first := RequestWindowStorage(t.Context(), owner, "history", initial.ViewRef, initial.Roots[0].RootRef)
	if first.State != "ready" || first.Inspection == nil || !first.Inspection.ReadAllowed || !first.Inspection.Roots[0].Consented {
		t.Fatal("trusted selected request did not set the two gates", first)
	}
	before, err := settings.InspectSessionReads()
	if err != nil || !reflect.DeepEqual(before.Reads.Agents, map[string]bool{"fixture": true, "codex": false, "claude": true}) || before.Reads.All {
		t.Fatal("agent consent replaced neighbors", before, err)
	}
	second := RequestWindowStorage(t.Context(), owner, "history", first.Inspection.ViewRef, first.Inspection.Roots[1].RootRef)
	if second.State != "ready" || second.Inspection == nil || !second.Inspection.Roots[0].Consented || !second.Inspection.Roots[1].Consented {
		t.Fatal("second exact root replaced its neighbor", second)
	}
	released := ReleaseWindowStorage(t.Context(), owner, "history", second.Inspection.ViewRef, second.Inspection.Roots[0].ConsentRef)
	if released.State != "released" || released.Inspection == nil || released.Inspection.Roots[0].Consented ||
		!released.Inspection.Roots[1].Consented || !released.Inspection.ReadAllowed {
		t.Fatal("release affected another row or ordinary consent", released)
	}
	rebound := RequestWindowStorage(t.Context(), owner, "history", released.Inspection.ViewRef, released.Inspection.Roots[0].RootRef)
	if rebound.State != "ready" || rebound.Inspection.ViewRef == second.Inspection.ViewRef {
		t.Fatal("rebind reused old consent revision", rebound)
	}
	stale := ReleaseWindowStorage(t.Context(), owner, "history", second.Inspection.ViewRef, second.Inspection.Roots[0].ConsentRef)
	if stale.State != "changed" || stale.Inspection == nil || !stale.Inspection.Roots[0].Consented {
		t.Fatal("old consent accepted after revoke/rebind ABA", stale)
	}
	after, err := settings.InspectSessionReads()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("root lifecycle rewrote agent consent", before, after, err)
	}
}

func TestWindowStorageTwoStepFailureReturnsActualGates(t *testing.T) {
	_, owner := windowStorageFixture(t)
	initial, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil {
		t.Fatal(err)
	}
	windowStorageBeforeReadGate = func() {
		if err := SetReads(nil, map[string]bool{"codex": true}); err != nil {
			t.Fatal(err)
		}
	}
	result := RequestWindowStorage(t.Context(), owner, "history", initial.ViewRef, initial.Roots[0].RootRef)
	if result.State != "changed" || result.Inspection == nil || result.Inspection.ReadAllowed || !result.Inspection.Roots[0].Consented {
		t.Fatal("partial failure claimed ready or concealed persisted root consent", result)
	}
	current, err := settings.InspectSessionReads()
	if err != nil || !current.Reads.Reads("codex") || current.Reads.Reads("fixture") {
		t.Fatal("failed second step overwrote independent consent", current, err)
	}
}

func TestWindowStorageRequestWithSettingsBackedOwnOff(t *testing.T) {
	_, owner := windowStorageFixture(t)
	previous := OwnOff.Load()
	check := func() bool { return settings.Shared().GatewayMode == "on" }
	OwnOff.Store(&check)
	t.Cleanup(func() { OwnOff.Store(previous) })
	inspection, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil {
		t.Fatal(err)
	}
	result := RequestWindowStorage(t.Context(), owner, "history", inspection.ViewRef, inspection.Roots[0].RootRef)
	if result.State != "ready" || result.Inspection == nil || !result.Inspection.ReadAllowed ||
		!result.Inspection.Roots[0].Consented {
		t.Fatal("settings-backed gateway check prevented the independent read-gate CAS", result)
	}
}

func TestWindowStorageExternalConsentABAInvalidatesView(t *testing.T) {
	_, owner := windowStorageFixture(t)
	initial, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil {
		t.Fatal(err)
	}
	granted := RequestWindowStorage(t.Context(), owner, "history", initial.ViewRef, initial.Roots[0].RootRef)
	if granted.State != "ready" || granted.Inspection == nil {
		t.Fatal(granted)
	}
	before, err := os.ReadFile(storageConsentsPath())
	if err != nil {
		t.Fatal(err)
	}
	epoch := sourceEpoch.Load()
	if err := edit.WriteAtomic(storageConsentsPath(), []byte("[]")); err != nil {
		t.Fatal(err)
	}
	if err := edit.WriteAtomic(storageConsentsPath(), before); err != nil {
		t.Fatal(err)
	}
	if sourceEpoch.Load() != epoch {
		t.Fatal("fixture unexpectedly used the in-process consent owner")
	}
	result := ReleaseWindowStorage(t.Context(), owner, "history", granted.Inspection.ViewRef, granted.Inspection.Roots[0].ConsentRef)
	if result.State != "changed" || result.Inspection == nil || !result.Inspection.Roots[0].Consented {
		t.Fatal("file identity/content CAS accepted a foreign revoke/rebind ABA", result)
	}
}
func TestWindowStorageChangesInvalidateInspectedAuthority(t *testing.T) {
	for _, change := range []string{"package", "config", "root", "off", "gateway", "consent-file", "settings"} {
		t.Run(change, func(t *testing.T) {
			manager, owner := windowStorageFixture(t)
			inspection, err := InspectWindowStorage(t.Context(), owner, "history")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "package":
				owner.PackageRevision = strings.Repeat("f", 64)
			case "config":
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), owner.Module, owner.ConfigVersion, json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "root":
				home, _ := os.UserHomeDir()
				path := filepath.Join(home, ".window-history/first")
				if err := os.Rename(path, path+".previous"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "off":
				if err := manager.SetEnabled(t.Context(), owner.Module, false); err != nil {
					t.Fatal(err)
				}
			case "gateway":
				previous := OwnOff.Load()
				denied := func() bool { return true }
				OwnOff.Store(&denied)
				t.Cleanup(func() { OwnOff.Store(previous) })
			case "consent-file":
				if err := os.MkdirAll(filepath.Dir(storageConsentsPath()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(storageConsentsPath(), []byte(`[]`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "settings":
				if err := SetReads(nil, map[string]bool{"codex": true}); err != nil {
					t.Fatal(err)
				}
			}
			result := RequestWindowStorage(t.Context(), owner, "history", inspection.ViewRef, inspection.Roots[0].RootRef)
			if result.State == "ready" || result.State == "released" {
				t.Fatal("changed tuple accepted old inspection", change, result)
			}
		})
	}
}

func TestWindowStorageReadRevisionBindsContentAndMedia(t *testing.T) {
	result := sdk.StorageResult{Parts: []sdk.SessionSourcePart{{Role: "user", Kind: "text", Text: "hello"},
		{Role: "user", Kind: "image", Name: "selected", Text: "[Attached image/png: image.png]"}},
		Media: []sdk.StorageMedia{{ID: "selected", Name: "../private\\folder\\im\x00age.png", MIME: "image/png", Bytes: []byte("one")}}}
	first := projectWindowStorage(result, "view", "session")
	result.SnapshotRef = "new invocation lease is not a content revision"
	second := projectWindowStorage(result, "view", "session")
	if first.ReadRef != second.ReadRef || first.Parts[1].Name != first.Media[0].MediaRef {
		t.Fatal("revision uses invocation identity or lost selected media reference", first, second)
	}
	if first.Media[0].Name != "image.png" || len(windowStorageMediaName(strings.Repeat("界", 200))) > 256 {
		t.Fatal("media descriptor exposed a path, control character or unbounded name", first.Media[0])
	}
	for _, change := range []string{"text", "bytes", "mime", "length", "view", "session"} {
		t.Run(change, func(t *testing.T) {
			body, _ := json.Marshal(result)
			var changed sdk.StorageResult
			json.Unmarshal(body, &changed)
			view, session := "view", "session"
			switch change {
			case "text":
				changed.Parts[0].Text = "other"
			case "bytes":
				changed.Media[0].Bytes = []byte("two")
			case "mime":
				changed.Media[0].MIME = "text/plain"
			case "length":
				changed.Media[0].Bytes = []byte("longer")
			case "view":
				view = "other"
			case "session":
				session = "other"
			}
			if projectWindowStorage(changed, view, session).ReadRef == first.ReadRef {
				t.Fatal("changed normalized read retained media authority")
			}
		})
	}
	oversized := sdk.StorageResult{Parts: []sdk.SessionSourcePart{{Role: "user", Kind: "text", Text: strings.Repeat("界", transcriptMax)}}}
	bounded := projectWindowStorage(oversized, "view", "session")
	if !bounded.Cut || len(bounded.Parts) != 1 || len(bounded.Parts[0].Text) > transcriptMax || bounded.Parts[0].Cut == 0 {
		t.Fatal("normalized text exceeded browser ceiling", bounded.Cut, len(bounded.Parts))
	}
}
