package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAgentGroupPriceRechecksReplacedExtensionAccount(t *testing.T) {
	host := &extensionModelRevisionBarrier{fixtureExtensionProviderHost: fixtureExtensionProvider(t, true)}
	providerExtensionHost = func() extensionProviderHost { return host }
	priceCatalog(t)

	const firstModel = "fixture-group-price-first"
	const otherModel = "fixture-group-price-other"
	host.manifest.Contract.Capabilities = []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}
	first := host.manifest.Providers[0]
	first.Models[0].ID = firstModel
	first.Models[0].Price = &sdk.ModelPrice{Input: 3, Output: 7}
	other := first
	other.ID, other.Name = "other", "Other fixture vendor"
	other.Models = []sdk.Model{{ID: otherModel, Price: &sdk.ModelPrice{Input: 11, Output: 17}}}
	host.manifest.Providers = append(host.manifest.Providers, other)
	if err := host.manifest.Validate(); err != nil {
		t.Fatal(err)
	}

	if err := host.store.DeleteAccount(host.manifest.ID, first.ID, "bob", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := host.store.SaveAccount(extensionhost.Account{
		Plugin: host.manifest.ID, Provider: other.ID, ID: "carol", Auth: json.RawMessage(`{"access":"private-carol"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Fatalf("unexpected SDK call %s for %s/%s", method, scope.Provider, scope.Account)
		return nil, nil
	}

	firstID, _, _ := ExtensionPublicProvider(host.manifest.ID, first.ID)
	otherID, _, _ := ExtensionPublicProvider(host.manifest.ID, other.ID)
	const groupID = "fixture-price"
	if err := SaveGroup(Group{ID: groupID, Routing: Ordered, Members: []string{firstID + "/" + firstModel, otherID + "/" + otherModel}}); err != nil {
		t.Fatal(err)
	}
	defer Hold()()
	entries, _ := CatalogFor("claude")
	ref := GroupPrefix + groupID
	index := slices.IndexFunc(entries, func(entry Entry) bool { return entry.ID == ref && entry.Group == groupID })
	if index < 0 {
		t.Fatalf("saved group %s is absent from the agent catalog", ref)
	}
	find := FoundOnce(GroupFinder())
	_, members, ok := find(ref)
	if !ok || len(members) != 2 || members[0].Provider.ID != firstID || members[0].Model != firstModel ||
		members[1].Provider.ID != otherID || members[1].Model != otherModel {
		t.Fatalf("unexpected group members: %+v (found %t)", members, ok)
	}
	captured := members[0].Provider
	scope, ok := captured.ExtensionScope()
	if !ok || scope.Account != "alice" || scope.Generation != 1 {
		t.Fatalf("unexpected first account selection: %+v (found %t)", scope, ok)
	}

	st := settings.Load().ForAgent("claude")
	quote := AgentPricer(st, find)
	if price, known := quote(entries[index]); !known || price.Input != 3 || price.Output != 7 {
		t.Fatalf("stable mixed-group quote = %+v (known %t), want 3/7", price, known)
	}
	peer := extensionhost.NewStore(host.namespace)
	var replacement extensionhost.Account
	host.beforeConfig = func() {
		host.beforeConfig = func() {
			var err error
			replacement, err = peer.SaveAccount(extensionhost.Account{
				Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"access":"replacement"}`),
			}, scope.Generation)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	price, known := quote(entries[index])
	if replacement.Generation != scope.Generation+1 {
		t.Fatalf("peer replacement did not publish during the other member's quote: generation %d", replacement.Generation)
	}
	if known {
		t.Errorf("mixed-group quote after peer replacement = %+v, want unknown", price)
	}
	if price, known := EffectivePriceIn(st, otherID, otherModel); !known || price.Input != 11 || price.Output != 17 {
		t.Fatalf("unrelated member quote = %+v (known %t), want 11/17", price, known)
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, captured.Chat+"/chat/completions",
		strings.NewReader(`{"model":"`+firstModel+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = captured.Do(http.DefaultClient, request)
	var refusal *sdk.Error
	if !errors.As(err, &refusal) || refusal.Code != "stale_generation" {
		t.Fatalf("captured account dispatch = %v, want stale_generation", err)
	}
	if price, known := EffectivePriceIn(st, firstID, firstModel); !known || price.Input != 3 || price.Output != 7 {
		t.Fatalf("fresh first member quote = %+v (known %t), want 3/7", price, known)
	}
}
