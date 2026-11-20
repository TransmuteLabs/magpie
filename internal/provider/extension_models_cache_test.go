package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

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
	extensionModelCache.Delete(first.cacheKey())
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
	extensionModelCache.Delete(first.Account.extension.cacheKey())
	if cold := extensionAccounts()[0]; cold.Account.Lists("not-entitled") {
		t.Fatal("cold lookup treated a measured empty list as unknown")
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
	extensionModelCache.Delete(first.cacheKey())
	listed, err := extensionAccounts()[0].Account.extension.models()
	if err != nil || len(listed) != 1 || listed[0].ID != "m1" {
		t.Fatalf("late refresh replaced the earlier catalog: %+v, %v", listed, err)
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
	extensionModelCache.Delete(binding.cacheKey())
	actual, err := binding.models()
	if err != nil || len(actual) != len(models) || actual[len(actual)-1].ID != models[len(models)-1].ID {
		t.Fatal("chunked catalog lost a model", len(actual), err)
	}
	scope := sdk.Scope{Plugin: host.manifest.ID}
	root, err := host.store.StateGet(scope, binding.modelCatalogKey())
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
	extensionModelCache.Delete(binding.cacheKey())
	if _, err := binding.models(); err == nil {
		t.Fatal("corrupt catalog chunk became a usable partial model list")
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
	extensionModelCache.Delete(binding.cacheKey())
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
