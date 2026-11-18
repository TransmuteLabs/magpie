package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type configCatalogProviderHost struct {
	*fixtureExtensionProviderHost
}

func (host *configCatalogProviderHost) ModuleConfig(ctx context.Context, id string) (sdk.ConfigSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	value, err := host.store.StateGet(sdk.Scope{Plugin: id}, "module.config")
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if value.Version == 0 {
		value.Value = json.RawMessage(`{}`)
	}
	return sdk.ConfigSnapshot{Version: strconv.FormatUint(value.Version, 10), Values: value.Value}, nil
}

func TestExtensionExternalConfigCASInvalidatesHeldCatalog(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(strconv.FormatBool(authenticated), func(t *testing.T) {
			fixture := fixtureExtensionProvider(t, authenticated)
			plugin.UseCached(nil)
			root := filepath.Dir(extensionhost.CatalogPath())
			original := fixture.store
			fixture.store, fixture.namespace = extensionhost.NewStore(root), root
			if authenticated {
				for _, id := range []string{"alice", "bob"} {
					account, err := original.GetAccount(fixture.manifest.ID, "vendor", id)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := fixture.store.SaveAccount(account, 0); err != nil {
						t.Fatal(err)
					}
				}
			}
			host := &configCatalogProviderHost{fixture}
			providerExtensionHost = func() extensionProviderHost { return host }
			oldHeld, oldKept := heldFor, keptFor
			heldFor, keptFor = time.Hour, time.Hour
			t.Cleanup(func() { heldFor, keptFor = oldHeld, oldKept })
			calls := 0
			host.call = func(_ context.Context, method string, _ sdk.Scope, _ any,
				consume func(extensionhost.Event) error) (json.RawMessage, error) {
				if method == sdk.MethodProviderModels {
					return json.RawMessage(`{"models":[{"id":"before-config","protocols":["chat"]}]}`), nil
				}
				calls++
				if err := consume(extensionhost.Event{Name: "headers", Data: json.RawMessage(`{"status":204}`)}); err != nil {
					return nil, err
				}
				return json.RawMessage(`{"status":204,"outcome":"completed"}`), nil
			}
			initial := extensionAccounts()[0]
			if _, err := initial.Account.extension.fetchModels(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot := func() (Provider, Stamp, []string) {
				t.Helper()
				defer Hold()()
				var selected Provider
				for _, candidate := range All() {
					if candidate.ID == initial.ID {
						selected = candidate
					}
				}
				if selected.Account == nil {
					t.Fatal("extension provider disappeared")
				}
				var models []string
				for _, entry := range Catalog() {
					if strings.HasPrefix(entry.ID, initial.ID+"/") {
						models = append(models, entry.Model)
					}
				}
				return selected, StampNow(), models
			}
			old, stamp, models := snapshot()
			if !slices.Contains(models, "before-config") {
				t.Fatal("initial held catalog did not contain its fetched model")
			}
			release := Hold()
			defer release()
			foreign := extensionhost.NewStore(root)
			scope := sdk.Scope{Plugin: host.manifest.ID}
			if _, err := foreign.StateCAS(scope, "unrelated", 0, json.RawMessage(`{"value":1}`)); err != nil {
				t.Fatal(err)
			}
			if _, same, _ := snapshot(); same != stamp {
				t.Fatal("unrelated private state invalidated the held catalog")
			}
			if _, err := foreign.StateCAS(scope, "module.config", 0, json.RawMessage(`{"enabled":true}`)); err != nil {
				t.Fatal(err)
			}
			if err := old.Account.extension.checkModelRevision(t.Context()); err == nil {
				t.Fatal("old selected catalog adopted the external config transition")
			}
			current, next, models := snapshot()
			if next == stamp || current.Account.extension.modelKey == old.Account.extension.modelKey ||
				slices.Contains(models, "before-config") {
				t.Fatal("external config CAS retained the held routing catalog and old configuration models")
			}
			beforeScope, _ := old.ExtensionScope()
			afterScope, _ := current.ExtensionScope()
			if beforeScope != afterScope {
				t.Fatal("config transition changed the selected account generation")
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				current.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
			if err != nil {
				t.Fatal(err)
			}
			response, err := current.Do(http.DefaultClient, request)
			if err != nil {
				t.Fatal("fresh ordinary execution reused the stale routing selection", err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent || calls != 1 {
				t.Fatalf("fresh config execution = %d, calls = %d", response.StatusCode, calls)
			}
			if _, unchanged, _ := snapshot(); unchanged != next {
				t.Fatal("unchanged config moved the catalog stamp")
			}
		})
	}
}
