package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/yetone/magpie/internal/catalog"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionProviderHost interface {
	Namespace() string
	List() ([]extensionhost.Installed, error)
	ListContext(context.Context) ([]extensionhost.Installed, error)
	Store() *extensionhost.Store
	ModuleConfig(context.Context, string) (sdk.ConfigSnapshot, error)
	PublishModelCatalog(context.Context, extensionhost.Installed, string, sdk.Scope, string, uint64, json.RawMessage) (extensionhost.VersionedValue, error)
	Call(context.Context, string, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error)
}

var providerExtensionHost = func() extensionProviderHost { return extensionhost.Default() }

type extensionAccount struct {
	name             string
	host             extensionProviderHost
	manifest         sdk.Manifest
	descriptor       sdk.ProviderDescriptor
	scope            sdk.Scope
	public           extensionPublicIdentity
	modelKey         string
	enableGeneration string
	generation       *atomic.Uint64
}

func (login Login) ExtensionAccountName() string {
	if login.extension != nil {
		return login.extension.name
	}
	return ""
}

func ExtensionProviderID(module, provider string) string {
	id, _, _ := ExtensionPublicProvider(module, provider)
	return id
}

// ExtensionProviders discovers providers from usable SDK modules. Shared
// catalog or account policy errors make the entire result unavailable.
func ExtensionProviders() ([]Provider, error) {
	providers, _, err := extensionProvidersWithPolicy()
	return providers, err
}

func extensionProvidersWithPolicy() ([]Provider, []savedLogin, error) {
	return providerExtensionsContext(context.Background())
}

func providerExtensionsContext(ctx context.Context) ([]Provider, []savedLogin, error) {
	return extensionProvidersWithPolicyContext(ctx, providerExtensionHost(),
		func() extensionSubscriptionOwners { return heldSubscriptionOwnersContext(ctx) })
}

func extensionProvidersWithPolicyContext(ctx context.Context, host extensionProviderHost, ownerSnapshot func() extensionSubscriptionOwners) ([]Provider, []savedLogin, error) {
	installed, err := activeProviderModules(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	out := []Provider{}
	owners := sync.OnceValue(ownerSnapshot)
	for _, entry := range installed {
		if !entry.Manifest.Contract.Has(sdk.OutboundExecutor) &&
			!slices.ContainsFunc(entry.Manifest.Providers, func(provider sdk.ProviderDescriptor) bool { return provider.AccountOnly() }) {
			continue
		}
		providers, err := discoverExtensionModuleContext(ctx, host, entry, owners)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			log.Printf("extension providers unavailable for %s: %v", entry.Manifest.ID, err)
			continue
		}
		out = append(out, providers...)
	}
	logins, err := extensionPolicies(out)
	if err != nil {
		return nil, nil, err
	}
	return out, logins, nil
}

func enabledExtensionModule(ctx context.Context, host extensionProviderHost, id string) (extensionhost.Installed, error) {
	if registry, ok := host.(interface {
		ActiveModule(context.Context, string) (extensionhost.Installed, error)
	}); ok {
		return registry.ActiveModule(ctx, id)
	}
	entries, err := activeProviderModules(ctx, host)
	if err != nil {
		return extensionhost.Installed{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id {
			return entry, nil
		}
	}
	return extensionhost.Installed{}, errors.New("extension is not enabled")
}

func discoverExtensionModuleContext(ctx context.Context, host extensionProviderHost, entry extensionhost.Installed, ownerSnapshot func() extensionSubscriptionOwners) ([]Provider, error) {
	manifest := entry.Manifest.Clone()
	authenticated := manifest.Contract.Has(sdk.AccountAuth)
	if authenticated && settings.AccountDiscoveryOff() {
		return nil, nil
	}
	modelRevision, err := extensionModelRevision(ctx, host, entry)
	if err != nil {
		return nil, fmt.Errorf("read module configuration: %w", err)
	}
	var out []Provider
	owners := ownerSnapshot()
	if owners.err != nil {
		return nil, owners.err
	}
	descriptors := slices.DeleteFunc(slices.Clone(manifest.Providers), func(descriptor sdk.ProviderDescriptor) bool {
		return !owners.identity(sdk.Scope{Plugin: manifest.ID, Provider: descriptor.ID}).visible ||
			!manifest.Contract.Has(sdk.OutboundExecutor) && !descriptor.AccountOnly()
	})
	var accounts map[string][]extensionhost.AccountMetadata
	if authenticated {
		accounts, err = extensionAccountMetadata(ctx, host, manifest.ID, descriptors, true)
		if err != nil {
			return nil, fmt.Errorf("read accounts for %s: %w", manifest.ID, err)
		}
	}
	for _, descriptor := range descriptors {
		scope := sdk.Scope{Plugin: manifest.ID, Provider: descriptor.ID}
		if !authenticated {
			out = append(out, extensionProvider(&extensionAccount{host: host, manifest: manifest, descriptor: descriptor,
				scope: scope, public: owners.identity(scope), modelKey: modelRevision, enableGeneration: entry.EnableGeneration}))
			continue
		}
		identities := map[string]bool{}
		for _, account := range accounts[descriptor.ID] {
			scope.Account, scope.Generation = account.ID, account.Generation
			identity := owners.identity(scope)
			key := accountKey(identity.user)
			if identities[key] {
				return nil, fmt.Errorf("account IDs for %s are ambiguous under core account policy", descriptor.ID)
			}
			identities[key] = true
			out = append(out, extensionProvider(&extensionAccount{host: host, manifest: manifest, descriptor: descriptor,
				name:  account.Name,
				scope: scope, public: identity,
				modelKey: modelRevision, enableGeneration: entry.EnableGeneration}))
		}
	}
	return out, nil
}

func extensionAccountMetadata(ctx context.Context, host extensionProviderHost, module string, descriptors []sdk.ProviderDescriptor, includeNames bool) (map[string][]extensionhost.AccountMetadata, error) {
	if len(descriptors) == 0 {
		return nil, nil
	}
	providers := make([]string, len(descriptors))
	for index, descriptor := range descriptors {
		providers[index] = descriptor.ID
	}
	return host.Store().AccountsMetadataContext(ctx, module, providers, includeNames)
}

func extensionPolicies(providers []Provider) ([]savedLogin, error) {
	if settings.AccountDiscoveryOff() {
		return nil, nil
	}
	logins, err := readLoginsFile()
	if err != nil {
		return nil, err
	}
	positions := make(map[extensionLoginKey]int, len(logins))
	for index, login := range logins {
		positions[extensionLoginKey{login.Agent, login.User}] = index
	}
	for _, provider := range providers {
		if provider.Account.extension.scope.Account == "" {
			continue
		}
		if position, exists := positions[extensionKey(provider.Account.extension.identity())]; exists {
			provider.Account.Plan = logins[position].Plan
		}
	}
	var unsaved savedLogin
	sort.SliceStable(providers, func(left, right int) bool {
		a, b := providers[left], providers[right]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		first, second := a.Account.extension.identity(), b.Account.extension.identity()
		leftPosition, leftSaved := positions[extensionKey(first)]
		rightPosition, rightSaved := positions[extensionKey(second)]
		leftLogin, rightLogin := &unsaved, &unsaved
		if leftSaved {
			leftLogin = &logins[leftPosition]
		}
		if rightSaved {
			rightLogin = &logins[rightPosition]
		}
		if leftLogin.First != rightLogin.First && first.revision != "" {
			return leftLogin.First
		}
		if leftLogin.Order == rightLogin.Order && first.revision != "" {
			return leftSaved && (!rightSaved || leftPosition < rightPosition)
		}
		return leftLogin.Order > 0 && (rightLogin.Order == 0 || leftLogin.Order < rightLogin.Order)
	})
	return logins, nil
}

func extensionAccounts() []Provider {
	return extensionAccountsContext(context.Background())
}

func extensionAccountsContext(ctx context.Context) []Provider {
	providers, _, err := providerExtensionsContext(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("extension providers unavailable: %v", err)
		}
		return nil
	}
	seen := map[string]int{}
	out := []Provider{}
	for _, provider := range providers {
		if index, exists := seen[provider.ID]; !exists {
			seen[provider.ID] = len(out)
			out = append(out, provider)
		} else if !out[index].Account.extension.on() && provider.Account.extension.on() {
			out[index] = provider
		}
	}
	for index := range out {
		primary := out[index]
		out[index].Account.extensionList = func(ctx context.Context) []catalog.Model {
			return extensionServedModelsContext(ctx, primary, providers)
		}
	}
	return out
}

func extensionServedModelsContext(ctx context.Context, primary Provider, providers []Provider) []catalog.Model {
	out, _ := primary.Account.extension.modelsContext(ctx)
	seen := map[string]bool{}
	for _, model := range out {
		seen[model.ID] = true
	}
	for _, provider := range providers {
		if ctx.Err() != nil {
			return nil
		}
		if provider.ID != primary.ID || provider.Account.User == primary.Account.User || !provider.Account.extension.on() {
			continue
		}
		models, err := provider.Account.extension.modelsContext(ctx)
		if err != nil {
			continue
		}
		for _, model := range models {
			if !seen[model.ID] {
				seen[model.ID] = true
				out = append(out, model)
			}
		}
	}
	return out
}

func extensionProvider(binding *extensionAccount) Provider {
	binding.generation = &atomic.Uint64{}
	binding.generation.Store(binding.scope.Generation)
	binding.public = binding.identity()
	id, user := binding.public.provider, binding.public.user
	if user == "" {
		user = binding.descriptor.Name
	}
	account := &Account{Agent: id, User: user, extension: binding, magpieList: true, listedOnly: true}
	account.extensionList = func(ctx context.Context) []catalog.Model {
		models, _ := binding.modelsContext(ctx)
		return models
	}
	account.models = func() []catalog.Model { return account.extensionList(context.Background()) }
	account.fetch = binding.fetchModels
	account.transport = binding.execute
	provider := Provider{ID: id, Name: binding.descriptor.Name, Account: account}
	base := "extension://" + id
	if binding.manifest.Contract.Version.Minor >= 7 && binding.descriptor.SupportsOperation(sdk.OperationSystemOne) {
		provider.Decide = base + "/v1"
	}
	for _, protocol := range binding.descriptor.Protocols {
		switch extensionProtocol(string(protocol)) {
		case Chat:
			provider.Chat = base + "/v1"
		case Responses:
			provider.Responses = base + "/v1"
		case Anthropic:
			provider.Anthropic = base
		case Gemini:
			provider.Gemini = base + "/v1beta"
		}
	}
	return provider
}

func extensionProtocol(protocol string) Protocol {
	if protocol == "messages" {
		return Anthropic
	}
	return Protocol(protocol)
}

func (binding *extensionAccount) check(ctx context.Context) error {
	return binding.checkSelected(ctx, false)
}

func (binding *extensionAccount) checkSelected(ctx context.Context, exactRevision bool) error {
	_, err := binding.checkedSelection(ctx, exactRevision)
	return err
}

func (binding *extensionAccount) checkedEntry(ctx context.Context, exactRevision bool) (extensionhost.Installed, error) {
	selected, err := binding.checkedSelection(ctx, exactRevision)
	return selected.entry, err
}

type extensionSelection struct {
	entry     extensionhost.Installed
	scope     sdk.Scope
	expiresAt int64
}

func (binding *extensionAccount) checkedSelection(ctx context.Context, exactRevision bool) (extensionSelection, error) {
	if err := ctx.Err(); err != nil {
		return extensionSelection{}, err
	}
	if binding.manifest.Contract.Has(sdk.AccountAuth) && settings.AccountDiscoveryOff() {
		return extensionSelection{}, errAccountDiscoveryOff
	}
	if err := CheckExtensionSubscription(ctx, binding.scope.Plugin, binding.scope.Provider, binding.identity().revision); err != nil {
		return extensionSelection{}, err
	}
	var entry extensionhost.Installed
	var err error
	if exactRevision {
		entry, err = binding.modelRevisionEntry(ctx)
	} else {
		entry, err = enabledExtensionModule(ctx, binding.host, binding.scope.Plugin)
	}
	if err != nil {
		return extensionSelection{}, err
	}
	current := entry.Manifest
	if !slices.ContainsFunc(current.Providers, func(descriptor sdk.ProviderDescriptor) bool {
		return descriptor.ID == binding.scope.Provider && descriptor.AccountOnly() == binding.descriptor.AccountOnly() &&
			(current.Contract.Has(sdk.OutboundExecutor) || descriptor.AccountOnly() && current.Contract.Has(sdk.AccountAuth))
	}) {
		return extensionSelection{}, errors.New("extension provider is unavailable")
	}
	scope := binding.currentScope()
	selected := extensionSelection{entry: entry, scope: scope}
	if scope.Account != "" {
		account, err := binding.host.Store().RefreshedAccountContext(ctx, scope)
		if err != nil {
			extensionSelectionFailure(ctx, scope, err)
			return extensionSelection{}, err
		}
		if binding.generation != nil {
			binding.generation.CompareAndSwap(scope.Generation, account.Generation)
		} else if account.Generation != scope.Generation {
			return extensionSelection{}, errors.New("extension account generation changed")
		}
		selected.scope.Generation = account.Generation
		selected.expiresAt = account.ExpiresAt
	}
	return selected, nil
}

func (binding *extensionAccount) currentScope() sdk.Scope {
	scope := binding.scope
	if binding.generation != nil {
		scope.Generation = binding.generation.Load()
	}
	return scope
}

func (binding *extensionAccount) cacheKey() string {
	scope := binding.currentScope()
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s", binding.host.Namespace(), scope.Plugin, scope.Provider,
		scope.Account, scope.Generation, binding.modelKey)
	if revision := binding.identity().revision; revision != "" {
		key += "\x00" + revision
	}
	return key
}

func loginReadingCacheKey(login Login) string {
	return loginReadingCacheKeyContext(context.Background(), login)
}

func loginReadingCacheKeyContext(ctx context.Context, login Login) string {
	if login.extension != nil {
		return extensionUsageKey(ctx, login.extension)
	}
	return login.Agent + "/" + strings.ToLower(login.User)
}

func loginReadingUserKey(agent, user string) string {
	if logins, external := extensionLogins(agent); external {
		for _, login := range logins {
			if strings.EqualFold(login.User, user) {
				return loginReadingCacheKey(login)
			}
		}
	}
	return agent + "/" + strings.ToLower(user)
}

func extensionUsageBindings(agent string) (map[string]string, bool) {
	logins, external := extensionLogins(agent)
	if !external {
		return nil, false
	}
	bindings := map[string]string{}
	for _, login := range logins {
		bindings[login.User] = loginReadingCacheKey(login)
	}
	return bindings, true
}

func syncExtensionAllowanceBindings(agent string, bindings map[string]string) {
	syncAllowanceBindings(&usedCache.extensions, agent, bindings)
}

func (binding *extensionAccount) models() ([]catalog.Model, error) {
	return binding.modelsContext(context.Background())
}

func (binding *extensionAccount) modelsContext(ctx context.Context) ([]catalog.Model, error) {
	models, _, err := binding.selectedModels(ctx)
	return models, err
}

func (binding *extensionAccount) selectedModels(ctx context.Context) ([]catalog.Model, bool, error) {
	snapshot := *binding
	snapshot.scope, snapshot.generation = binding.currentScope(), nil
	binding = &snapshot
	if err := binding.checkSelected(ctx, true); err != nil {
		return nil, false, err
	}
	if binding.descriptor.AccountOnly() {
		return nil, false, nil
	}
	if !binding.manifest.Contract.Has(sdk.ModelDiscovery) {
		models, err := binding.projectModels(binding.descriptor.Models)
		return models, false, err
	}
	snapshotModels, publish, err := binding.readModelCatalog(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := binding.checkSelected(ctx, true); err != nil {
		return nil, false, err
	}
	if publish {
		extensionModelCache.Store(snapshotModels)
	}
	models, known := snapshotModels.models, snapshotModels.rootVersion != 0
	if !known {
		models = slices.DeleteFunc(slices.Clone(binding.descriptor.Models), func(model sdk.Model) bool { return model.Kind == "decision" })
	}
	projected, err := binding.projectModels(models)
	return projected, known, err
}

func (binding *extensionAccount) projectModels(models []sdk.Model) ([]catalog.Model, error) {
	out := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if model.ContextWindow < 0 || model.MaxContextWindow < 0 || model.MaxOutput < 0 ||
			int64(int(model.ContextWindow)) != model.ContextWindow || int64(int(model.MaxContextWindow)) != model.MaxContextWindow ||
			int64(int(model.MaxOutput)) != model.MaxOutput {
			return nil, errors.New("extension model has invalid limits")
		}
		protocols := model.Protocols
		if len(protocols) == 0 && model.Kind != "decision" {
			protocols = binding.descriptor.Protocols
		}
		entry := catalog.Model{ID: model.ID, Name: model.Name, Provider: binding.identity().provider,
			Context: catalog.Window(int(model.ContextWindow)), Output: int(model.MaxOutput), Images: model.Vision,
			MaxContext: int(model.MaxContextWindow), Released: model.Released, Temperature: model.Temperature,
			InputModalities: slices.Clone(model.InputModalities), OutputModalities: slices.Clone(model.OutputModalities),
			Tiers: slices.Clone(model.ServiceTiers), Fast: slices.Contains(model.ServiceTiers, "priority"),
			Free: model.Free, Rate: model.Rate, RateWas: model.RateWas,
			AlwaysThinks: model.AlwaysThinks, DynamicTools: model.DynamicTools, DefaultEffort: model.DefaultEffort,
			Reasoning: model.Reasoning || model.AlwaysThinks || len(model.Efforts) > 0, Efforts: slices.Clone(model.Efforts)}
		if model.Temperature != nil {
			temperature := *model.Temperature
			entry.Temperature = &temperature
		}
		if model.InputModalities != nil {
			images := slices.Contains(model.InputModalities, "image")
			entry.Images, entry.ImageInput = images, &images
		}
		switch model.Kind {
		case "decision":
			entry.Decides = binding.manifest.Contract.Version.Minor >= 7 &&
				binding.descriptor.SupportsOperation(sdk.OperationSystemOne) && slices.Contains(model.Operations, sdk.OperationSystemOne)
		case "image":
			entry.Draws = true
		case "video":
			entry.Films = true
		case "embedding", "rerank", "speech", "transcription":
			entry.Serves = model.Kind
		}
		if model.Price != nil {
			entry.Price = &catalog.Price{Input: model.Price.Input, Output: model.Price.Output,
				CacheRead: model.Price.CacheRead, CacheWrite: model.Price.CacheWrite, CacheWrite1h: model.Price.CacheWrite1h}
		}
		for _, protocol := range protocols {
			entry.APIs = append(entry.APIs, string(extensionProtocol(string(protocol))))
		}
		out = append(out, entry)
	}
	return out, nil
}

func (binding *extensionAccount) fetchModels(ctx context.Context) ([]catalog.Model, error) {
	if binding.descriptor.AccountOnly() {
		return binding.modelsContext(ctx)
	}
	prepared, err := binding.prepare(ctx)
	if err != nil {
		return nil, err
	}
	binding = prepared
	if !binding.manifest.Contract.Has(sdk.ModelDiscovery) {
		return binding.modelsContext(ctx)
	}
	if binding.manifest.Contract.Version.Minor < 9 {
		if err := binding.checkModelRevision(ctx); err != nil {
			return nil, err
		}
	}
	before, err := binding.host.Store().StateGetContext(ctx, sdk.Scope{Plugin: binding.scope.Plugin}, binding.modelCatalogKey())
	if err != nil {
		return nil, err
	}
	ctx, err = extensionNetworkContext(extensionhost.WithSubscriptionOwnerRevision(ctx, binding.identity().revision), binding.scope)
	if err != nil {
		return nil, err
	}
	var result sdk.ModelsResult
	raw, err := binding.host.Call(ctx, binding.scope.Plugin, sdk.MethodProviderModels, binding.scope, sdk.ModelsRequest{}, nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, errors.New("invalid extension model catalog")
	}
	if err := binding.validateModels(result.Models); err != nil {
		return nil, fmt.Errorf("invalid extension model catalog: %w", err)
	}
	snapshotModels, err := binding.writeModelCatalog(ctx, result, before)
	if err == nil {
		if snapshotModels.rootVersion != before.Version {
			extensionModelCache.Store(snapshotModels)
			Changed()
		}
	} else if conflict, ok := errors.AsType[*sdk.Error](err); !ok || conflict.Code != "conflict" {
		return nil, err
	}
	return binding.modelsContext(ctx)
}

func (p Provider) IsExtension() bool { return p.Account != nil && p.Account.extension != nil }

func (p Provider) ExtensionScope() (sdk.Scope, bool) {
	if !p.IsExtension() {
		return sdk.Scope{}, false
	}
	return p.Account.extension.currentScope(), true
}

func (p Provider) extensionAPIs(model string) []Protocol {
	models := p.Account.models()
	model = p.OwnModelID(model)
	for _, entry := range models {
		if entry.ID == model {
			out := make([]Protocol, len(entry.APIs))
			for index, protocol := range entry.APIs {
				out[index] = Protocol(protocol)
			}
			return out
		}
	}
	return nil
}

func (p Provider) extensionAlsoOn() []Provider {
	return p.extensionAlsoOnContext(context.Background())
}

func (p Provider) extensionAlsoOnContext(ctx context.Context) []Provider {
	all, _, err := providerExtensionsContext(ctx)
	if err != nil {
		return nil
	}
	out := []Provider{}
	for _, other := range all {
		if other.ID == p.ID && other.Account.User != p.Account.User && other.Account.extension.on() {
			out = append(out, other)
		}
	}
	return out
}

func extensionLogins(agent string) ([]Login, bool) {
	return extensionLoginsContext(context.Background(), agent)
}

func extensionLoginsContext(ctx context.Context, agent string) ([]Login, bool) {
	if strings.HasPrefix(agent, "extension-") {
		return listExtensionLoginsContext(ctx, agent), true
	}
	owners := heldSubscriptionOwnersContext(ctx)
	if ctx.Err() != nil {
		return nil, true
	}
	if _, owned := owners.byPublicID(agent); !owned {
		return nil, false
	}
	return listExtensionLoginsContext(ctx, agent), true
}

// listExtensionLogins accepts an empty agent to list every SDK account.
func listExtensionLogins(agent string) []Login {
	return listExtensionLoginsContext(context.Background(), agent)
}

func listExtensionLoginsContext(ctx context.Context, agent string) []Login {
	providers, saved, err := providerExtensionsContext(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("extension logins unavailable: %v", err)
		}
		return nil
	}
	return extensionLoginsFrom(providers, saved, agent)
}

func extensionLoginsFrom(providers []Provider, saved []savedLogin, agent string) []Login {
	records := make(map[extensionLoginKey]*savedLogin, len(saved))
	for index := range saved {
		record := &saved[index]
		key := extensionLoginKey{record.Agent, record.User}
		if _, exists := records[key]; !exists {
			records[key] = record
		}
	}
	active := map[string]bool{}
	out := []Login{}
	for _, provider := range providers {
		binding := provider.Account.extension
		if binding.scope.Account == "" || agent != "" && provider.ID != agent {
			continue
		}
		login := Login{Agent: provider.ID, User: provider.Account.User, On: true, extension: binding,
			ExtensionGeneration: strconv.FormatUint(binding.currentScope().Generation, 10)}
		if record := records[extensionKey(binding.identity())]; record != nil {
			if record.Hidden != "" {
				continue
			}
			login.On = extensionPolicyOn(*record)
			login.Paused, login.Seen, login.Plan = record.Paused, record.Seen, record.Plan
			login.Lapsed = extensionAuthLapse(binding, *record)
		}
		if login.On && !active[login.Agent] {
			login.Active = true
			active[login.Agent] = true
		}
		out = append(out, login)
	}
	return out
}

func extensionQuota(ctx context.Context, login Login) SubscriptionQuota {
	quota := SubscriptionQuota{Provider: login.Agent, User: login.User, Windows: []QuotaWindow{}}
	binding := login.extension
	if binding == nil || !binding.manifest.Contract.Has(sdk.UsageReporter) {
		quota.Error = "extension provider does not report account usage"
		return quota
	}
	quota.Name = binding.descriptor.Name
	prepared, err := binding.prepare(ctx)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), netproxy.Held(ctx) != nil || ctx.Err() != nil
		return quota
	}
	binding = prepared
	request, proof, err := extensionUsageRequest(ctx, binding)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), true
		return quota
	}
	quota.extensionUsage = proof
	ctx, err = extensionNetworkContext(extensionhost.WithSubscriptionOwnerRevision(ctx, binding.identity().revision), binding.scope)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), netproxy.Held(ctx) != nil
		return quota
	}
	authRead, err := binding.beginUsageAuth(ctx)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), true
		return quota
	}
	revision, config, _ := strings.Cut(binding.modelKey, "\x00")
	ctx = extensionhost.WithProviderUsageSelection(ctx, revision, config, binding.enableGeneration)
	raw, err := binding.host.Call(ctx, binding.scope.Plugin, sdk.MethodProviderUsage, binding.scope, request, nil)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), ctx.Err() != nil
		var rejection *sdk.Error
		if errors.As(err, &rejection) {
			quota.ReadDeferred = quota.ReadDeferred || rejection.Code == "usage_stale" ||
				rejection.Code == "store_locked" || rejection.Code == "account_discovery_off"
		}
		if extensionUsageUnauthenticated(err) {
			if publishErr := binding.finishUsageAuth(ctx, authRead, true); publishErr != nil {
				quota.ReadDeferred = true
			}
		}
		return quota
	}
	if err := binding.check(ctx); err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), ctx.Err() != nil
		return quota
	}
	result, fresh, err := sdk.ValidateUsageResult(binding.manifest, raw)
	if err != nil {
		quota.Error = err.Error()
		return quota
	}
	if proof != nil {
		proof.fresh = fresh
		if binding.manifest.Contract.Version.Minor >= 4 && result.Revision != proof.stamp.Version() {
			quota.Error, quota.ReadDeferred = "usage result revision did not match its read", true
			return quota
		}
		if !proof.currentContext(ctx) {
			quota.Error, quota.ReadDeferred = "usage read preceded an invalidation or lacks a fresh acknowledgment", true
			if err := ctx.Err(); err != nil {
				quota.Error = err.Error()
			}
			return quota
		}
	}
	quota.Error, quota.ReadDeferred = extensionUsageError(result.Error), result.ReadDeferred
	quota.AsOf = extensionUsageTime(result.AsOf)
	var presence struct {
		Windows []struct {
			Used json.RawMessage `json:"used"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(raw, &presence); err != nil || len(presence.Windows) != len(result.Windows) {
		quota.Error = "extension provider returned an invalid usage result"
		return quota
	}
	for index, window := range result.Windows {
		used := bytes.TrimSpace(presence.Windows[index].Used)
		window.Unknown = window.Unknown || len(used) == 0 || bytes.Equal(used, []byte("null")) || window.Error != ""
		if window.Name == "" || !window.Unknown && (math.IsNaN(window.Used) || math.IsInf(window.Used, 0) || window.Used < 0 || window.Used > 100) {
			quota.Windows, quota.Error = nil, "extension provider returned an invalid quota window"
			return quota
		}
		window.Error = extensionUsageError(window.Error)
		entry := projectUsageWindow(window)
		entry.amountPresence, entry.balanceInformational = quotaAmountAbsent, true
		if window.Amount != nil {
			entry.Amount, entry.amountPresence = *window.Amount, quotaAmountPresent
		}
		if window.Balance != nil {
			entry.Balance = new(*window.Balance)
		}
		if set := idSetOf(window.Models); set != nil {
			entry.matches = set.has
		} else if set := idSetOf(window.NotModels); set != nil {
			entry.matches = func(model string) bool { return !set.has(model) }
		}
		entry.AsOf = extensionUsageTime(window.AsOf)
		entry.ResetsAt = extensionUsageTime(window.ResetsAt)
		if entry.AsOf != nil {
			if !entry.Text {
				if entry.Display == "" {
					entry.Display = fmt.Sprintf("%g%%", entry.Used)
				}
				entry.Text, entry.Aside, entry.noShare = true, true, true
			}
			entry.Display += " (as of " + entry.AsOf.Format(time.RFC3339Nano) + ")"
		}
		quota.Windows = append(quota.Windows, entry)
	}
	if fresh && quota.Error == "" && !quota.ReadDeferred {
		if err := binding.finishUsageAuth(ctx, authRead, false); err != nil {
			quota.Error, quota.ReadDeferred = err.Error(), true
		}
	}
	return quota
}

func extensionUsageTime(milliseconds int64) *time.Time {
	if milliseconds == 0 {
		return nil
	}
	return new(time.UnixMilli(milliseconds).UTC())
}

func extensionUsageError(value string) string {
	value = strings.TrimSpace(strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value))
	characters := []rune(value)
	return string(characters[:min(len(characters), 512)])
}

func extensionAccountOn(agent, user string) bool {
	return extensionPolicyAccountOn(extensionLoginPolicyAgent(agent), user)
}

func (binding *extensionAccount) on() bool {
	return extensionPolicyAccountOn(binding.identity().policyAgent, binding.identity().user)
}

func extensionPolicyAccountOn(policyAgent, user string) bool {
	if settings.AccountDiscoveryOff() {
		return true
	}
	logins, err := readLoginsFile()
	if err != nil {
		return false
	}
	for _, saved := range logins {
		if saved.Agent == policyAgent && saved.User == user {
			return extensionPolicyOn(saved)
		}
	}
	return true
}

func ForgetExtensionLogin(agent string, account sdk.AccountSelection) error {
	return ForgetExtensionLoginAt(context.Background(), agent, account, "")
}

func ForgetExtensionLoginAt(ctx context.Context, agent string, account sdk.AccountSelection, revision string) error {
	if settings.AccountDiscoveryOff() {
		return errAccountDiscoveryOff
	}
	generation, err := account.Validate()
	if err != nil {
		return err
	}
	return forgetExtensionLoginSelected(ctx, agent, account.ID, generation, &revision)
}

func forgetExtensionLogin(agent, user string, expectedGeneration uint64) error {
	return forgetExtensionLoginSelected(context.Background(), agent, user, expectedGeneration, nil)
}

func forgetExtensionLoginSelected(ctx context.Context, agent, user string, expectedGeneration uint64, revision *string) error {
	logins, _ := extensionLogins(agent)
	for _, login := range logins {
		if login.User != user {
			continue
		}
		scope := login.extension.currentScope()
		if expectedGeneration != 0 && scope.Generation != expectedGeneration {
			return errors.New("extension account generation changed")
		}
		identity := login.extension.identity()
		if revision != nil && *revision != identity.revision {
			return errSubscriptionOwnerChanged
		}
		deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := LogoutExtensionAuth(extensionhost.WithSubscriptionOwnerRevision(deadline, identity.revision), scope)
		cancel()
		if err != nil {
			return err
		}
		if identity.revision != "" {
			return nil
		}
		if !loginsMu.LockChecked() {
			return errSubscriptionLoginsLocked
		}
		defer loginsMu.Unlock()
		saved, err := readLoginsFile()
		if err != nil {
			return err
		}
		return writeLogins(slices.DeleteFunc(saved, identity.hasPolicy))
	}
	return errors.New("extension account is unavailable")
}

func extensionUsageFetches(ctx context.Context, via func(string) context.Context, hidden map[string]bool) []func() SubscriptionQuota {
	providers, saved, err := providerExtensionsContext(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("extension usage unavailable: %v", err)
		}
		return nil
	}
	out := []func() SubscriptionQuota{}
	logins := extensionLoginsFrom(providers, saved, "")
	for len(logins) > 0 {
		first := logins[0]
		count := 1
		for count < len(logins) && logins[count].Agent == first.Agent {
			count++
		}
		group := logins[:count]
		logins = logins[count:]
		if hidden[first.Agent] || leftAlone(first.Agent) || !first.extension.manifest.Contract.Has(sdk.UsageReporter) {
			continue
		}
		ctx := via(first.Agent)
		if refresh, ok := refreshing(ctx); ok && refresh.provider != first.Agent {
			continue
		}
		out = append(out, perLogin(ctx, group, cardFace{name: first.extension.descriptor.Name})...)
	}
	return out
}
