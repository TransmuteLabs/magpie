package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestExtensionCatalogProjectionUsesOrdinaryKeyModelAndVisibilityPolicy(t *testing.T) {
	providerFileHome(t)
	configured := Provider{ID: "catalog", Name: "Catalog", Chat: "http://127.0.0.1:1/v1",
		Key: "private-first-token", Keys: []KeyAccount{{Key: "private-second-token"}, {Key: "private-disabled-token", Off: true}},
		Models: []string{"m1", "m2"}, AccountModels: map[string][]string{
			KeyID("private-first-token"): {"m1"}, KeyID("private-second-token"): {"m2"},
		}}
	if err := Save(configured); err != nil {
		t.Fatal(err)
	}
	if err := Save(Provider{ID: "hidden", Name: "Hidden", Chat: "http://127.0.0.1:1/v1",
		Key: "hidden-token", Models: []string{"m1"}, Unlisted: true}); err != nil {
		t.Fatal(err)
	}
	result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err != nil || len(result.Accounts) != 2 {
		t.Fatal("ordinary catalog/key visibility did not reach the projection", result, err)
	}
	for index, account := range result.Accounts {
		if account.Provider != "catalog" || account.Kind != "key" || len(account.Models) != 1 ||
			account.Models[0].ID != "catalog/m"+strconv.Itoa(index+1) || account.Quota.Known {
			t.Fatal("projection ignored account restrictions or invented a quota", result)
		}
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{"private-", "hidden-token", "127.0.0.1", "Authorization", `"key"` + ":"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("catalog projection exposed private provider configuration", string(encoded))
		}
	}
	if err := SetAccountModels("catalog", KeyID("private-first-token"), []string{"m2"}); err != nil {
		t.Fatal(err)
	}
	updated, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err != nil || len(updated.Accounts) != 2 || updated.Accounts[0].Models[0].ID != "catalog/m2" ||
		updated.Accounts[0].Ref != result.Accounts[0].Ref {
		t.Fatal("ordinary account policy changed without changing its projection", updated, err)
	}
}

func TestExtensionCatalogProjectionKeepsAccountSelectionAndNeverFetchesVendor(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Providers[0].Models = append(host.manifest.Providers[0].Models, sdk.Model{ID: "m2", ContextWindow: 256000})
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Error("catalog projection asked a vendor instead of its saved catalog")
		return nil, nil
	}
	providers := extensionAccounts()
	if len(providers) != 1 {
		t.Fatal("fixture did not create its ordinary provider")
	}
	id := providers[0].ID
	if err := SetAccountModels(id, "alice", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if err := SetAccountModels(id, "bob", []string{"m2"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"alice", "bob"} {
		scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: account, Generation: 1}
		result, err := extensionCatalogProjection(t.Context(), scope, sdk.CatalogAccessDescriptor{Scope: "selected"})
		want := "m1"
		if account == "bob" {
			want = "m2"
		}
		if err != nil || len(result.Accounts) != 1 || len(result.Accounts[0].Models) != 1 ||
			result.Accounts[0].Models[0].ID != id+"/"+want {
			t.Fatal("selected catalog account used another account's models", result, err)
		}
	}
}

type countingCatalogProjectionHost struct {
	*fixtureExtensionProviderHost
	selections atomic.Int32
}

func (host *countingCatalogProjectionHost) ActiveModuleConfig(ctx context.Context, id string) (extensionhost.Installed, sdk.ConfigSnapshot, error) {
	host.selections.Add(1)
	config, err := host.ModuleConfig(ctx, id)
	return extensionhost.Installed{Manifest: host.manifest, Revision: "fixture"}, config, err
}

func TestExtensionCatalogProjectionModelSelectionDoesNotScaleWithModels(t *testing.T) {
	project := func(t *testing.T, modelCount int) int32 {
		t.Helper()
		host := &countingCatalogProjectionHost{fixtureExtensionProviderHost: fixtureExtensionProvider(t, true)}
		models := make([]sdk.Model, 0, modelCount)
		picks := make([]string, 0, modelCount)
		for index := range modelCount {
			id := "m" + strconv.Itoa(index+1)
			models = append(models, sdk.Model{ID: id})
			picks = append(picks, id)
		}
		host.manifest.Providers[0].Models = models
		providerExtensionHost = func() extensionProviderHost { return host }
		Changed()
		providerID := ExtensionProviderID(host.manifest.ID, "vendor")
		if err := SetModels(providerID, picks); err != nil {
			t.Fatal(err)
		}
		host.selections.Store(0)
		Changed()

		result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
			sdk.CatalogAccessDescriptor{Scope: "inventory"})
		accounts := 0
		for _, account := range result.Accounts {
			if account.Provider != providerID {
				continue
			}
			accounts++
			if len(account.Models) != modelCount {
				t.Fatalf("account %q has %d models, want %d", account.Ref, len(account.Models), modelCount)
			}
		}
		if err != nil || accounts != 2 {
			t.Fatalf("projected %d SDK accounts, error %v", accounts, err)
		}
		selections := host.selections.Load()
		t.Logf("SDK configuration selections for %d models: %d", modelCount, selections)
		return selections
	}
	baseline := project(t, 1)
	if baseline == 0 {
		t.Fatal("fixture did not exercise SDK selection")
	}
	for _, modelCount := range []int{10, 100} {
		t.Run(strconv.Itoa(modelCount), func(t *testing.T) {
			if selections := project(t, modelCount); selections != baseline {
				t.Fatalf("SDK model selection count = %d, want one-model baseline %d", selections, baseline)
			}
		})
	}
}

func TestExtensionCatalogProjectionDistinguishesUnknownEmptyAndUnreadableAccountModels(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Providers[0].Models = nil
	access := sdk.CatalogAccessDescriptor{Scope: "selected"}
	alice := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	bob := alice
	bob.Account = "bob"
	providers, err := ExtensionProviders()
	if err != nil || len(providers) != 2 {
		t.Fatalf("fixture providers = %d, %v", len(providers), err)
	}
	binding := func(scope sdk.Scope) *extensionAccount {
		t.Helper()
		for _, provider := range providers {
			if candidate := provider.Account.extension; candidate.currentScope() == scope {
				return candidate
			}
		}
		t.Fatalf("missing fixture account %s", scope.Account)
		return nil
	}
	peer := extensionhost.NewStore(host.Namespace())
	seed := func(scope sdk.Scope, modelIDs ...string) {
		t.Helper()
		selected := *binding(scope)
		external := *host
		external.store = peer
		selected.host = &external
		module := sdk.Scope{Plugin: scope.Plugin}
		before, err := peer.StateGetContext(t.Context(), module, selected.modelCatalogKey())
		if err != nil {
			t.Fatal(err)
		}
		models := make([]sdk.Model, 0, len(modelIDs))
		for _, id := range modelIDs {
			models = append(models, sdk.Model{ID: id})
		}
		if _, err := selected.writeModelCatalog(t.Context(), sdk.ModelsResult{Models: models}, before); err != nil {
			t.Fatal(err)
		}
	}
	seed(bob, "beta", "alpha")
	Changed()
	providerID := ExtensionProviderID(bob.Plugin, bob.Provider)
	if _, changed, err := SetModelID(providerID+"/beta", "local-beta"); err != nil || !changed {
		t.Fatalf("name vendor model locally: changed %t, error %v", changed, err)
	}
	project := func(scope sdk.Scope, wantModels ...string) string {
		t.Helper()
		result, err := extensionCatalogProjection(t.Context(), scope, access)
		if len(wantModels) == 0 {
			if err == nil || len(result.Accounts) != 0 {
				t.Fatalf("projected an account with no eligible models: %+v, %v", result, err)
			}
			return ""
		}
		if err != nil || len(result.Accounts) != 1 {
			t.Fatalf("selected projection: %+v, %v", result, err)
		}
		projected := result.Accounts[0]
		got := make([]string, 0, len(projected.Models))
		for _, model := range projected.Models {
			got = append(got, model.ID)
		}
		if !slices.Equal(got, wantModels) || projected.Kind != "account" || projected.Quota.Known {
			t.Fatalf("selected models = %v, want %v (account %+v)", got, wantModels, projected)
		}
		return projected.Ref
	}

	aliceRef := project(alice, providerID+"/local-beta", providerID+"/alpha")
	bobRef := project(bob, providerID+"/local-beta", providerID+"/alpha")
	if aliceRef == bobRef {
		t.Fatal("separate account scopes shared an opaque reference")
	}
	seed(alice)
	project(alice)

	module := sdk.Scope{Plugin: alice.Plugin}
	key := binding(alice).modelCatalogKey()
	current, err := peer.StateGetContext(t.Context(), module, key)
	if err != nil || current.Version == 0 {
		t.Fatalf("missing known-empty model root: %+v, %v", current, err)
	}
	if _, err := peer.StateCASContext(t.Context(), module, key, current.Version, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	project(alice)
	if ref := project(bob, providerID+"/local-beta", providerID+"/alpha"); ref != bobRef {
		t.Fatal("Alice's unreadable model list changed Bob's identity")
	}
	seed(alice, "alpha")
	if ref := project(alice, providerID+"/alpha"); ref != aliceRef {
		t.Fatal("replacing Alice's model list changed her identity")
	}
}

func TestExtensionCatalogProjectionPreservesQuotaApplicabilityAndUnknown(t *testing.T) {
	readAt, reset := time.Unix(100, 0), time.Unix(200, 0)
	models := []sdk.CatalogModelProjection{{ID: "vendor/opus"}, {ID: "vendor/sonnet"}}
	projected := projectedCatalogQuota(Provider{}, SubscriptionQuota{ReadAt: &readAt, Windows: []QuotaWindow{
		{Name: "shared", Used: 0, ResetsAt: &reset},
		{Name: "opus", Used: 90, Model: "opus"},
		{Name: "unknown", Unknown: true},
		{Name: "extra", Aside: true, Credits: true, Unlimited: true},
	}}, models)
	if !projected.Known || projected.AsOf != readAt.UnixMilli() || len(projected.Windows) != 4 ||
		projected.Windows[0].Used == nil || *projected.Windows[0].Used != 0 ||
		projected.Windows[0].ResetsAt != reset.UnixMilli() || len(projected.Windows[0].Models) != 2 ||
		len(projected.Windows[1].Models) != 1 || projected.Windows[1].Models[0] != "vendor/opus" ||
		projected.Windows[2].Used != nil || !projected.Windows[3].Aside || !projected.Windows[3].Credits || !projected.Windows[3].Unlimited {
		t.Fatal("quota projection changed measured zero, applicability or unknown evidence", projected)
	}
	if projected := projectedCatalogQuota(Provider{}, SubscriptionQuota{Error: "private vendor failure",
		Windows: []QuotaWindow{{Name: "daily", Used: 0}}}, models); projected.Known || len(projected.Windows) != 0 {
		t.Fatal("a failed quota read became an empty allowance", projected)
	}
}

type catalogProjectionQuotaHost struct {
	*extensionhost.Manager
	lateModule string
	onLate     func()
	calls      atomic.Int32
}

func (host *catalogProjectionQuotaHost) ActiveModuleConfig(ctx context.Context, id string) (extensionhost.Installed, sdk.ConfigSnapshot, error) {
	entry, config, err := host.Manager.ActiveModuleConfig(ctx, id)
	if err == nil && id == host.lateModule && host.onLate != nil {
		host.onLate()
	}
	return entry, config, err
}

func (host *catalogProjectionQuotaHost) Call(context.Context, string, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
	host.calls.Add(1)
	return nil, errors.New("catalog projection must use cached readings")
}

func TestExtensionCatalogProjectionBatchesCurrentQuotaProofs(t *testing.T) {
	providerFileHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	const module, lateModule, vendor, lateVendor = "fixture.catalog-usage", "fixture.catalog-late", "vendor", "late-vendor"
	manager := extensionhost.NewManager(filepath.Dir(extensionhost.CatalogPath()))
	t.Cleanup(func() { closeDiscoveryCatalogManager(t, manager) })
	previous := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return manager }
	t.Cleanup(func() {
		providerExtensionHost = previous
		Changed()
	})
	foreign := extensionhost.NewCatalog(manager.Namespace())
	install := func(module, vendor string, users ...string) {
		t.Helper()
		source := t.TempDir()
		manifest := sdk.Manifest{
			Schema: sdk.ManifestSchema, ID: module, Version: "1.0.0", Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
				Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth, sdk.UsageReporter}},
			Providers: []sdk.ProviderDescriptor{{ID: vendor, Name: "Fixture", Protocols: []protocolext.Protocol{protocolext.Messages},
				Models: []sdk.Model{{ID: "m1", Name: "Fixture model"}}}},
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("fixture, never executed"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := foreign.Install(t.Context(), source, func(ctx context.Context, _ extensionhost.Installed) error {
			return ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}
		for _, user := range users {
			if _, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: module, Provider: vendor, ID: user,
				Auth: json.RawMessage(`{"token":"synthetic"}`), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	install(module, vendor, "alice", "bob")
	install(lateModule, lateVendor, "late")
	providerID, lateID := ExtensionProviderID(module, vendor), ExtensionProviderID(lateModule, lateVendor)
	if err := SetOrder([]string{providerID, lateID}); err != nil {
		t.Fatal(err)
	}

	observed := time.Now().Add(-time.Minute)
	readings := map[string]SubscriptionQuota{}
	providers, err := ExtensionProviders()
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range providers {
		if selected.ID != providerID {
			continue
		}
		user := selected.Account.User
		_, proof, err := extensionUsageRequest(t.Context(), selected.Account.extension)
		if err != nil || proof == nil {
			t.Fatalf("warm usage proof for %s: %v", user, err)
		}
		used := map[string]float64{"alice": 20, "bob": 40}[user]
		readings[user] = SubscriptionQuota{Provider: providerID, User: user, ReadAt: &observed,
			Windows: []QuotaWindow{{Name: "Plan", Used: used}}, extensionUsage: proof}
	}
	alice, bob := readings["alice"], readings["bob"]
	if alice.extensionUsage == nil || bob.extensionUsage == nil {
		t.Fatal("fixture did not discover both quota accounts")
	}
	warmSubscriptionUsageForTest(t, alice, bob)

	host := &catalogProjectionQuotaHost{Manager: manager, lateModule: lateModule}
	providerExtensionHost = func() extensionProviderHost { return host }
	Changed()
	held.Lock()
	oldHeldFor := heldFor
	heldFor = time.Hour
	held.Unlock()
	t.Cleanup(func() {
		held.Lock()
		heldFor = oldHeldFor
		held.Unlock()
	})
	defer Hold()()
	const reader = "catalog-reader"
	project := func() sdk.CatalogReadResult {
		t.Helper()
		result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: reader}, sdk.CatalogAccessDescriptor{Scope: "inventory"})
		if err != nil || len(result.Accounts) != 3 || result.Accounts[2].Provider != lateID {
			t.Fatalf("expected two SDK accounts followed by the later selection: %+v, %v", result, err)
		}
		return result
	}
	before := project()
	row := func(result sdk.CatalogReadResult, user string) sdk.CatalogAccountProjection {
		t.Helper()
		ref := projectedCatalogAccount(reader, providerID, "account", user).Ref
		for _, account := range result.Accounts[:2] {
			if account.Ref == ref {
				return account
			}
		}
		t.Fatalf("catalog lost the %s account", user)
		return sdk.CatalogAccountProjection{}
	}
	for _, quota := range []SubscriptionQuota{alice, bob} {
		value := row(before, quota.User).Quota
		if !value.Known || value.AsOf != observed.UnixMilli() || len(value.Windows) != 1 ||
			value.Windows[0].Used == nil || *value.Windows[0].Used != quota.Windows[0].Used {
			t.Fatalf("fixture did not project warm quota for %s: %+v", quota.User, value)
		}
	}
	rawScope, err := json.Marshal(bob.extensionUsage.stamp.Scope)
	if err != nil {
		t.Fatal(err)
	}
	lateChecks := 0
	host.onLate = func() {
		lateChecks++
		if lateChecks != 1 {
			t.Fatal("the held catalog repeated the later model selection")
		}
		ctx := testenv.DeadlineContext(t, 15*time.Second)
		peer := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestExtensionUsageChild$", "-test.count=1")
		peer.Env = append(os.Environ(), "SDK_USAGE_CHILD_ROOT="+manager.Namespace(), "SDK_USAGE_CHILD_SCOPE="+string(rawScope))
		if output, err := peer.CombinedOutput(); err != nil {
			t.Fatalf("invalidate Bob from another process: %v\n%s", err, output)
		}
		stamp, err := manager.Store().UsageStampContext(ctx, bob.extensionUsage.stamp.Scope)
		if err != nil || !stamp.Dirty || stamp.Revision == bob.extensionUsage.stamp.Revision {
			t.Fatalf("peer failed to commit Bob's new usage revision: %+v, %v", stamp, err)
		}
	}
	after := project()
	if lateChecks != 1 || host.calls.Load() != 0 {
		t.Fatalf("late model selections = %d, worker calls = %d", lateChecks, host.calls.Load())
	}
	if got, want := row(after, alice.User), row(before, alice.User); !reflect.DeepEqual(got, want) {
		t.Fatalf("peer invalidation changed the unaffected sibling: got %+v, want %+v", got, want)
	}
	want := row(before, bob.User)
	want.Quota = sdk.CatalogQuotaProjection{}
	if got := row(after, bob.User); !reflect.DeepEqual(got, want) {
		t.Fatalf("peer invalidation must clear only Bob's quota: got %+v, want %+v", got, want)
	}
}

func TestExtensionCatalogProjectionQuotaProofHonorsDeadline(t *testing.T) {
	old, _, _ := prepareUsageReadFixture(t)
	id := ExtensionProviderID(old.manifest.ID, "vendor")
	quota := usageTestRead(t, id, "alice", 20)
	current := fixtureExtensionProvider(t, true)
	warmSubscriptionUsageForTest(t, quota)
	scope := sdk.Scope{Plugin: current.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	access := sdk.CatalogAccessDescriptor{Scope: "selected"}
	if result, err := extensionCatalogProjection(t.Context(), scope, access); err != nil ||
		len(result.Accounts) != 1 || !result.Accounts[0].Quota.Known {
		t.Fatalf("fixture did not reach its warm quota projection: %+v %v", result, err)
	}
	unlock, held := filelock.Lock(filepath.Join(old.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the quota proof Store lock")
	}
	synctest.Test(t, func(t *testing.T) {
		type projection struct {
			result sdk.CatalogReadResult
			err    error
		}
		done := make(chan projection, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := extensionCatalogProjection(ctx, scope, access)
			done <- projection{result, err}
		}()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			unlock()
			if got.err == nil || len(got.result.Accounts) != 0 {
				t.Errorf("a timed-out projection published quota: %+v %v", got.result, got.err)
			}
		default:
			unlock()
			<-done
			t.Error("projection remained blocked by the cached proof after its deadline")
		}
	})
}

func TestExtensionCatalogProjectionColdStoreHonorsDeadline(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	access := sdk.CatalogAccessDescriptor{Scope: "selected"}
	Changed()
	unlock, held := filelock.Lock(filepath.Join(host.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the current SDK Store")
	}
	synctest.Test(t, func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := extensionCatalogProjection(ctx, scope, access)
			if len(result.Accounts) != 0 || result.ReadAt != 0 {
				t.Errorf("a canceled read published a catalog: %+v", result)
			}
			done <- err
		}()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			unlock()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("cold catalog error = %v, want deadline exceeded", err)
			}
		default:
			unlock()
			<-done
			t.Error("cold catalog discovery still held the request after its deadline")
		}
	})
	result, err := extensionCatalogProjection(t.Context(), scope, access)
	if err != nil || len(result.Accounts) != 1 || len(result.Accounts[0].Models) != 1 ||
		result.Accounts[0].Models[0].ID != ExtensionProviderID(scope.Plugin, scope.Provider)+"/m1" {
		t.Fatalf("canceled discovery spoiled the next selected catalog: %+v %v", result, err)
	}
	scope.Generation++
	if result, err := extensionCatalogProjection(t.Context(), scope, access); err == nil || len(result.Accounts) != 0 {
		t.Fatalf("selected catalog accepted a different generation: %+v %v", result, err)
	}
}

func TestExtensionCatalogProjectionRefusesUnreadableProviderSource(t *testing.T) {
	providerFileHome(t)
	if err := os.MkdirAll(Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err == nil || result.ReadAt != 0 || len(result.Accounts) != 0 {
		t.Fatal("an unreadable provider source became an empty catalog", result, err)
	}
}
