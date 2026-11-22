package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
	"golang.org/x/sys/unix"
)

func TestSettingsDisabledHitsBoundsCatalogReads(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	manager := extension.Default()
	catalog := extension.NewCatalog(manager.Namespace())
	source := t.TempDir()
	const ownModel = "p20-wire-model"
	const disabledModel = "family/p20-disabled-model"
	const firstModel = "family/p20-disabled-first"
	const otherModel = "p20-still-served"
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "fixture.disabled-hits", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{
			Version:      sdk.APIVersion{Major: 1, Minor: 3},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor},
		},
		Providers: []sdk.ProviderDescriptor{{
			ID: "vendor", Name: "Disabled hits fixture", Protocols: []protocolext.Protocol{protocolext.Chat},
			Models: []sdk.Model{{ID: firstModel}, {ID: ownModel}, {ID: otherModel}},
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("settings must not execute a worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}

	id := provider.ExtensionProviderID(manifest.ID, "vendor")
	disabledID, otherID := id+"/"+disabledModel, id+"/"+otherModel
	firstID := id + "/" + firstModel
	if err := settings.Save(settings.Settings{Currency: "usd", ModelIDs: map[string]string{disabledID: ownModel}}); err != nil {
		t.Fatal(err)
	}
	const glob = "*P20-DISABLED*"
	h := Handler(nil, nil)
	read := func(t *testing.T) settingsJSON {
		t.Helper()
		reply := do(h, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/settings", nil))
		var view settingsJSON
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &view) != nil {
			t.Fatalf("GET /api/settings: %d %s", reply.Code, reply.Body)
		}
		return view
	}
	hasModel := func(view settingsJSON, id string) bool {
		return slices.ContainsFunc(view.TitleModels, func(model modelRef) bool { return model.ID == id })
	}
	unchanged := read(t)
	if unchanged.DisabledHits != nil || !hasModel(unchanged, firstID) || !hasModel(unchanged, disabledID) || !hasModel(unchanged, otherID) {
		t.Fatal("all fixture models must be available with no disabled patterns")
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, filepath.Join(catalog.Root, "catalog.json"), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	singleReads := 0
	for _, count := range []int{1, 10, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			patterns := []string{glob}
			hits := map[string][]string{glob: {firstID, disabledID}}
			if count > 1 {
				leaf, qualified := "p20-disabled-model", id+"/family/p20-disabled-mode?"
				patterns = append(patterns, leaf, qualified, ownModel)
				hits[leaf], hits[qualified] = []string{disabledID}, []string{disabledID}
			}
			for len(patterns) < count {
				patterns = append(patterns, "unmatched-p20-"+strconv.Itoa(len(patterns)))
			}
			if err := provider.SetDisabledModels(patterns); err != nil {
				t.Fatal(err)
			}
			read(t)
			catalogWindowReadEvents(t, fd)
			view := read(t)
			reads := catalogWindowReadEvents(t, fd)
			t.Logf("%d patterns: %d catalog reads", count, reads)
			if count == 1 {
				singleReads = reads
			}
			if singleReads < 1 || reads != singleReads {
				t.Errorf("%d patterns read catalog.json %d times, one pattern read it %d times", count, reads, singleReads)
			}
			if !slices.Equal(view.DisabledModels, patterns) || len(view.DisabledHits) != count {
				t.Fatalf("settings patterns: %v, hits: %v", view.DisabledModels, view.DisabledHits)
			}
			for _, pat := range patterns {
				want := hits[pat]
				if got, ok := view.DisabledHits[pat]; !ok || (got == nil) != (want == nil) || !slices.Equal(got, want) {
					t.Errorf("hits for %q: %v (present %t)", pat, got, ok)
				}
			}
			if hasModel(view, firstID) || hasModel(view, disabledID) || !hasModel(view, otherID) {
				t.Error("title model list did not exclude only the disabled fixture models")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if aborted := do(h, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/settings", nil)); aborted.Body.Len() != 0 {
		t.Errorf("canceled GET /api/settings published a reply: %s", aborted.Body)
	}
	if manager.Running(manifest.ID) {
		t.Fatal("settings discovery started the SDK worker")
	}
}
