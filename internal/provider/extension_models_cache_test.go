package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type indexedModelProviderHost struct {
	*fixtureExtensionProviderHost
	revision   string
	enablement string
}

func (host *indexedModelProviderHost) ListContext(context.Context) ([]extensionhost.Installed, error) {
	return nil, errors.New("selected fixture lookup enumerated the catalog")
}

func (host *indexedModelProviderHost) ModuleConfig(context.Context, string) (sdk.ConfigSnapshot, error) {
	return sdk.ConfigSnapshot{}, errors.New("selected fixture read package and configuration separately")
}

func (host *indexedModelProviderHost) ActiveModule(ctx context.Context, id string) (extensionhost.Installed, error) {
	manifest, err := host.Manifest(id)
	if err != nil {
		return extensionhost.Installed{}, err
	}
	return extensionhost.Installed{Manifest: manifest, Revision: host.revision, EnableGeneration: host.enablement}, ctx.Err()
}

func (host *indexedModelProviderHost) ActiveModuleConfig(ctx context.Context, id string) (extensionhost.Installed, sdk.ConfigSnapshot, error) {
	entry, err := host.ActiveModule(ctx, id)
	if err != nil {
		return extensionhost.Installed{}, sdk.ConfigSnapshot{}, err
	}
	if entry.Manifest.Contract.Version.Minor < 1 {
		return extensionhost.Installed{}, sdk.ConfigSnapshot{}, errors.New("fixture has no module configuration")
	}
	config, err := host.fixtureExtensionProviderHost.ModuleConfig(ctx, id)
	return entry, config, err
}

func TestExtensionModelRevisionUsesActiveEntry(t *testing.T) {
	for _, minor := range []int{0, 3} {
		t.Run("1."+strconv.Itoa(minor), func(t *testing.T) {
			fixture := fixtureExtensionProvider(t, false)
			fixture.manifest.Contract.Version.Minor = minor
			providers, err := ExtensionProviders()
			if err != nil || len(providers) != 1 {
				t.Fatalf("discovery: %d providers, %v", len(providers), err)
			}
			binding := providers[0].Account.extension
			binding.host = &indexedModelProviderHost{fixtureExtensionProviderHost: fixture, revision: "fixture"}
			models, err := binding.modelsContext(t.Context())
			if err != nil || len(models) != 1 || models[0].ID != "m1" {
				t.Fatalf("ordinary indexed model read: %+v, %v", models, err)
			}
			fixture.config = "1"
			err = binding.checkModelRevision(t.Context())
			if (err != nil) != (minor > 0) {
				t.Fatalf("configuration transition under SDK 1.%d: %v", minor, err)
			}
			fixture.disabled = true
			if _, err := binding.modelsContext(t.Context()); err == nil {
				t.Fatal("indexed model read used a disabled module")
			}
		})
	}
}

func TestExtensionModelRefreshStopsAtStoreDeadline(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	binding := extensionAccounts()[0].Account.extension
	if _, err := host.store.StateGet(sdk.Scope{Plugin: binding.scope.Plugin}, binding.modelCatalogKey()); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(host.namespace, "private", ".lock")
	lockReady := make(chan func(), 1)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		unlock, held := filelock.Lock(lockPath, time.Second)
		if !held {
			return nil, errors.New("fixture could not hold the SDK store lock")
		}
		lockReady <- unlock
		return json.RawMessage(`{"models":[{"id":"late-model"}]}`), nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := binding.fetchModels(ctx)
		done <- err
	}()
	var unlock func()
	select {
	case unlock = <-lockReady:
	case err := <-done:
		t.Fatalf("model refresh ended before its cache write: %v", err)
	}
	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("model refresh returned %v instead of the deadline", err)
		}
	case <-time.After(3 * time.Second):
		unlock()
		<-done
		t.Fatal("model refresh waited for the store after the deadline")
	}
	root, err := host.store.StateGet(sdk.Scope{Plugin: binding.scope.Plugin}, binding.modelCatalogKey())
	if err != nil || root.Version != 0 {
		t.Fatalf("canceled refresh published a cache root: %+v %v", root, err)
	}
}

func TestExtensionModelsSurviveColdCatalogLookup(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	calls := 0
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderModels {
			t.Fatalf("unexpected module invocation: %s", method)
		}
		calls++
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{{ID: "dynamic-only", ContextWindow: 65536}}})
	}
	first := extensionAccounts()[0].Account.extension
	if _, err := first.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	extensionModelCache.Delete(first.modelCatalogKey())
	cold := extensionAccounts()[0].Account.extension
	listed, err := cold.models()
	if err != nil || len(listed) != 1 || listed[0].ID != "dynamic-only" || listed[0].Context != 65536 {
		t.Fatalf("fresh catalog lost a discovered model: %+v, %v", listed, err)
	}
	if calls != 1 {
		t.Fatal("cold metadata lookup started another provider request", calls)
	}
}

func TestExtensionKnownEmptyModelsSurviveColdLookup(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.manifest.Providers[0].Models = nil
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{}})
	}
	first := extensionAccounts()[0]
	if !first.Account.Lists("unknown-before-refresh") {
		t.Fatal("never-fetched models became a known empty catalog")
	}
	if _, err := first.Account.extension.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	extensionModelCache.Delete(first.Account.extension.modelCatalogKey())
	if cold := extensionAccounts()[0]; cold.Account.Lists("not-entitled") {
		t.Fatal("cold lookup treated a measured empty list as unknown")
	}
}

func TestExtensionWarmModelCatalogFollowsForeignPublication(t *testing.T) {
	const writerRootEnv = "MAGPIE_TEST_MODEL_CATALOG_WRITER_ROOT"
	writerRoot := os.Getenv(writerRootEnv)
	host := fixtureExtensionProvider(t, writerRoot == "")
	model := "old-entitlement"
	if writerRoot != "" {
		host.store, host.namespace = extensionhost.NewStore(writerRoot), writerRoot
		host.manifest.Contract.Capabilities = append(host.manifest.Contract.Capabilities, sdk.AccountAuth)
		model = "new-entitlement"
	}
	calls := 0
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		calls++
		if calls != 1 || method != sdk.MethodProviderModels || scope.Account != "alice" || scope.Generation == 0 {
			t.Fatalf("unexpected model discovery: call %d, %s, scope %+v", calls, method, scope)
		}
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{{ID: model}}})
	}
	account := extensionAccounts()[0].Account
	if models, err := account.fetch(t.Context()); err != nil || len(models) != 1 || models[0].ID != model {
		t.Fatalf("model refresh did not publish %q: %+v, %v", model, models, err)
	}
	if writerRoot != "" {
		return
	}
	if !account.Lists("old-entitlement") || account.Lists("new-entitlement") {
		t.Fatal("old catalog did not warm before the second process")
	}
	binding := account.extension
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	key := binding.modelCatalogKey()
	before, err := host.store.StateGet(scope, key)
	if err != nil || before.Version == 0 {
		t.Fatalf("initial catalog root version: %d, %v", before.Version, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestExtensionWarmModelCatalogFollowsForeignPublication$", "-test.count=1")
	command.Env = append(os.Environ(), writerRootEnv+"="+host.namespace)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("other process did not publish models: %v\n%s", err, output)
	}
	after, err := host.store.StateGet(scope, key)
	if err != nil || after.Version != before.Version+1 {
		t.Fatalf("other process did not replace the same catalog root: %d -> %d, %v", before.Version, after.Version, err)
	}
	if old, current := account.Lists("old-entitlement"), account.Lists("new-entitlement"); old || !current {
		t.Fatalf("account has old=%t, current=%t after durable root changed %d -> %d", old, current, before.Version, after.Version)
	}
}

func TestExtensionWarmModelCatalogRejectsCorruptRootAtSameVersion(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.RawMessage(`{"models":[{"id":"discovered"}]}`), nil
	}
	account := extensionAccounts()[0].Account
	if _, err := account.fetch(t.Context()); err != nil || !account.Lists("discovered") {
		t.Fatalf("initial model discovery: %v", err)
	}
	key := account.extension.modelCatalogKey()
	files, err := filepath.Glob(filepath.Join(host.namespace, "private", host.manifest.ID, "state", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		if string(root["key"]) != strconv.Quote(key) {
			continue
		}
		root["value"] = json.RawMessage(`{}`)
		encoded, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if account.Lists("discovered") {
			t.Fatal("warm model memo used a corrupted root with a matching version")
		}
		return
	}
	t.Fatal("model root was not published")
}

func TestExtensionModelCacheEvictsOldSnapshotsWithoutLosingEntitlements(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.manifest.Providers[0].Models = nil
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.RawMessage(`{"models":[]}`), nil
	}
	account := extensionAccounts()[0].Account
	if _, err := account.extension.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := account.extension.modelCatalogKey()
	root, err := host.store.StateGet(sdk.Scope{Plugin: host.manifest.ID}, key)
	if err != nil || root.Version == 0 {
		t.Fatalf("initial catalog root version: %d, %v", root.Version, err)
	}
	digest := sha256.Sum256(root.Value)
	snapshot, found := extensionModelCache.Load(key, root.Version, digest)
	if !found {
		t.Fatal("refreshed catalog did not warm the memo")
	}
	keys := make([]string, 64)
	for index := range keys {
		keys[index] = fmt.Sprintf("%s-eviction-%d", key, index)
		snapshot.key = keys[index]
		extensionModelCache.Store(snapshot)
	}
	t.Cleanup(func() {
		for _, filler := range keys {
			extensionModelCache.Delete(filler)
		}
		extensionModelCache.Delete(key)
	})
	if _, found := extensionModelCache.Load(key, root.Version, digest); found {
		t.Fatal("an old model snapshot survived more than 64 newer snapshots")
	}
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Error("evicted snapshot triggered another provider request")
		return nil, errors.New("unexpected provider request")
	}
	if account.Lists("not-entitled") {
		t.Fatal("evicting the empty snapshot widened the account's entitlements")
	}
}

func TestExtensionStaticModelsDoNotReadDiscoveryRoot(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.manifest.Contract.Capabilities = slices.DeleteFunc(host.manifest.Contract.Capabilities, func(capability sdk.Capability) bool {
		return capability == sdk.ModelDiscovery
	})
	account := extensionAccounts()[0].Account
	scope := sdk.Scope{Plugin: host.manifest.ID}
	key := account.extension.modelCatalogKey()
	if _, err := host.store.StateCAS(scope, key, 0, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !account.Lists("m1") || account.Lists("not-listed") {
		t.Fatal("static model eligibility changed when discovery state was unreadable")
	}
}

func TestExtensionStaticModelsRejectChangedSelection(t *testing.T) {
	for _, change := range []string{"package", "configuration", "enablement"} {
		t.Run(change, func(t *testing.T) {
			fixture := fixtureExtensionProvider(t, false)
			fixture.manifest.Contract.Capabilities = slices.DeleteFunc(fixture.manifest.Contract.Capabilities, func(capability sdk.Capability) bool {
				return capability == sdk.ModelDiscovery
			})
			account := extensionAccounts()[0].Account
			host := &indexedModelProviderHost{fixtureExtensionProviderHost: fixture, revision: "fixture"}
			account.extension.host = host
			if !account.Lists("m1") {
				t.Fatal("current static model was unavailable")
			}
			switch change {
			case "package":
				host.revision = "replacement"
			case "configuration":
				fixture.config = "1"
			case "enablement":
				host.enablement = "re-enabled"
			}
			if account.Lists("m1") {
				t.Fatalf("held account still listed a static model after %s changed", change)
			}
		})
	}
}

func TestExtensionModelCacheSeparatesConfigurationVersions(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{{ID: "old-config-only"}}})
	}
	first := extensionAccounts()[0].Account.extension
	if _, err := first.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	host.config = "1"
	next := extensionAccounts()[0].Account.extension
	listed, err := next.models()
	if err != nil || len(listed) != 1 || listed[0].ID != "m1" || first.cacheKey() == next.cacheKey() {
		t.Fatalf("configuration change reused another configuration's model list: %+v, %v", listed, err)
	}
}

func TestExtensionLateModelRefreshCannotPublishIntoChangedConfiguration(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	first := extensionAccounts()[0].Account.extension
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		host.config = "1"
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{{ID: "late-model"}}})
	}
	if _, err := first.fetchModels(t.Context()); err == nil {
		t.Fatal("model refresh published after its selected configuration changed")
	}
	host.config = ""
	extensionModelCache.Delete(first.modelCatalogKey())
	listed, err := extensionAccounts()[0].Account.extension.models()
	if err != nil || len(listed) != 1 || listed[0].ID != "m1" {
		t.Fatalf("late refresh replaced the earlier catalog: %+v, %v", listed, err)
	}
}

func TestExtensionLateModelRefreshKeepsPublishedCatalog(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	account := extensionAccounts()[0].Account
	binding := account.extension
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	entered := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	host.call = func(ctx context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderModels {
			return nil, fmt.Errorf("unexpected module invocation: %s", method)
		}
		if requests.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
				return json.RawMessage(`{"models":[{"id":"old-model"}]}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return json.RawMessage(`{"models":[{"id":"new-model"}]}`), nil
	}
	var lateModels []string
	finished := make(chan struct{})
	var lateErr error
	go func() {
		defer close(finished)
		models, err := binding.fetchModels(ctx)
		lateErr = err
		for _, model := range models {
			lateModels = append(lateModels, model.ID)
		}
	}()
	defer func() {
		cancel()
		<-finished
	}()
	select {
	case <-entered:
	case <-finished:
		t.Fatalf("first refresh ended before discovery: %v", lateErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := binding.fetchModels(ctx); err != nil || !account.Lists("new-model") {
		t.Fatalf("concurrent discovery could not publish its model: %v", err)
	}
	close(release)
	<-finished
	if lateErr != nil || !slices.Equal(lateModels, []string{"new-model"}) || !account.Lists("new-model") || account.Lists("old-model") {
		t.Fatalf("late discovery replaced the selected catalog: %v, %v", lateModels, lateErr)
	}
}

func TestExtensionLargeCatalogChunksAndCorruptReads(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	models := make([]sdk.Model, 3000)
	for index := range models {
		models[index] = sdk.Model{ID: fmt.Sprintf("model-%04d", index), Name: strings.Repeat("x", 250)}
	}
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.Marshal(sdk.ModelsResult{Models: models})
	}
	binding := extensionAccounts()[0].Account.extension
	if _, err := binding.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	extensionModelCache.Delete(binding.modelCatalogKey())
	actual, err := binding.models()
	if err != nil || len(actual) != len(models) || actual[len(actual)-1].ID != models[len(models)-1].ID {
		t.Fatal("chunked catalog lost a model", len(actual), err)
	}
	scope := sdk.Scope{Plugin: host.manifest.ID}
	rootKey := binding.modelCatalogKey()
	root, err := host.store.StateGet(scope, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	var record extensionModelCatalog
	if err := json.Unmarshal(root.Value, &record); err != nil || len(record.Chunks) < 2 {
		t.Fatal("large catalog did not exercise multiple bounded chunks", err)
	}
	key := "models.chunk.v1." + record.Chunks[1]
	chunk, err := host.store.StateGet(scope, key)
	if err != nil {
		t.Fatal(err)
	}
	corrupt, _ := json.Marshal([]byte("truncated"))
	if _, err := host.store.StateCAS(scope, key, chunk.Version, corrupt); err != nil {
		t.Fatal(err)
	}
	if warm, err := binding.models(); err != nil || len(warm) != len(models) || warm[len(warm)-1].ID != models[len(models)-1].ID {
		t.Fatal("unchanged root did not reuse the validated snapshot", len(warm), err)
	}
	if _, err := host.store.StateCAS(scope, rootKey, root.Version, root.Value); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.models(); err == nil {
		t.Fatal("changed root accepted the old memo or a corrupt chunk")
	}
}

type extensionModelRevisionBarrier struct {
	*fixtureExtensionProviderHost
	beforeConfig func()
}

func (host *extensionModelRevisionBarrier) ModuleConfig(ctx context.Context, id string) (sdk.ConfigSnapshot, error) {
	if before := host.beforeConfig; before != nil {
		host.beforeConfig = nil
		before()
	}
	return host.fixtureExtensionProviderHost.ModuleConfig(ctx, id)
}

func seedExtensionModelCatalog(t *testing.T, authenticated bool) (*fixtureExtensionProviderHost, *extensionAccount) {
	t.Helper()
	host := fixtureExtensionProvider(t, authenticated)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{{ID: "generation-one-only"}}})
	}
	binding := extensionAccounts()[0].Account.extension
	if _, err := binding.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	extensionModelCache.Delete(binding.modelCatalogKey())
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Fatal("catalog metadata read started a provider or worker request")
		return nil, nil
	}
	return host, binding
}

func TestExtensionColdModelReadCannotPublishUnderLaterAccountGeneration(t *testing.T) {
	host, binding := seedExtensionModelCatalog(t, true)
	originalKey := binding.cacheKey()
	barrier := &extensionModelRevisionBarrier{fixtureExtensionProviderHost: host}
	binding.host = barrier
	barrier.beforeConfig = func() {
		previous := binding.currentScope()
		account, err := host.store.SaveAccount(extensionhost.Account{
			Plugin: previous.Plugin, Provider: previous.Provider, ID: previous.Account,
			Auth: json.RawMessage(`{"access":"generation-two"}`)}, previous.Generation)
		if err != nil {
			t.Fatal(err)
		}
		binding.generation.CompareAndSwap(previous.Generation, account.Generation)
	}
	binding.models()
	if binding.cacheKey() == originalKey {
		t.Fatal("fixture did not replace the selected account generation")
	}
	fresh := extensionAccounts()[0].Account.extension
	models, err := fresh.models()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model.ID == "generation-one-only" {
			t.Fatal("generation-one catalog was published under generation two")
		}
	}
}

func TestExtensionColdModelReadRejectsDeletedAccount(t *testing.T) {
	host, binding := seedExtensionModelCatalog(t, true)
	scope := binding.currentScope()
	if err := host.store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, scope.Generation); err != nil {
		t.Fatal(err)
	}
	if models, err := binding.models(); err == nil && len(models) != 0 {
		t.Fatal("deleted account retained access to its persisted model catalog")
	}
}

func TestExtensionWarmModelReadRejectsReplacedConfiguration(t *testing.T) {
	host, binding := seedExtensionModelCatalog(t, false)
	if _, err := binding.models(); err != nil {
		t.Fatal(err)
	}
	host.config = "1"
	if models, err := binding.models(); err == nil && len(models) != 0 {
		t.Fatal("held model catalog survived replacement of its configuration")
	}
}
