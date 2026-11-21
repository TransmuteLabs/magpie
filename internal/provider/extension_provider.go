package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	Call(context.Context, string, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error)
}

var providerExtensionHost = func() extensionProviderHost { return extensionhost.Default() }

type extensionAccount struct {
	name             string
	host             extensionProviderHost
	manifest         sdk.Manifest
	descriptor       sdk.ProviderDescriptor
	scope            sdk.Scope
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
	sum := sha256.Sum256([]byte(module + "\x00" + provider))
	return "extension-" + hex.EncodeToString(sum[:12])
}

// ExtensionProviders discovers providers from usable SDK modules. Shared
// catalog or account policy errors make the entire result unavailable.
func ExtensionProviders() ([]Provider, error) {
	host := providerExtensionHost()
	ctx := context.Background()
	installed, err := activeProviderModules(ctx, host)
	if err != nil {
		return nil, err
	}
	out := []Provider{}
	for _, entry := range installed {
		if !entry.Manifest.Contract.Has(sdk.OutboundExecutor) &&
			!slices.ContainsFunc(entry.Manifest.Providers, func(provider sdk.ProviderDescriptor) bool { return provider.AccountOnly() }) {
			continue
		}
		providers, err := discoverExtensionModuleContext(ctx, host, entry)
		if err != nil {
			log.Printf("extension providers unavailable for %s: %v", entry.Manifest.ID, err)
			continue
		}
		out = append(out, providers...)
	}
	return extensionPolicies(out)
}

func enabledExtensionModule(ctx context.Context, host extensionProviderHost, id string) (extensionhost.Installed, error) {
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

func discoverExtensionModuleContext(ctx context.Context, host extensionProviderHost, entry extensionhost.Installed) ([]Provider, error) {
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
	for _, descriptor := range manifest.Providers {
		if !manifest.Contract.Has(sdk.OutboundExecutor) && !descriptor.AccountOnly() {
			continue
		}
		if !authenticated {
			out = append(out, extensionProvider(&extensionAccount{host: host, manifest: manifest, descriptor: descriptor,
				scope: sdk.Scope{Plugin: manifest.ID, Provider: descriptor.ID}, modelKey: modelRevision, enableGeneration: entry.EnableGeneration}))
			continue
		}
		accounts, err := host.Store().AccountsContext(ctx, manifest.ID, descriptor.ID)
		if err != nil {
			return nil, fmt.Errorf("read accounts for %s: %w", descriptor.ID, err)
		}
		identities := map[string]bool{}
		for _, account := range accounts {
			identity := strings.ToLower(account.ID)
			if identities[identity] {
				return nil, fmt.Errorf("account IDs for %s are ambiguous under core account policy", descriptor.ID)
			}
			identities[identity] = true
			out = append(out, extensionProvider(&extensionAccount{host: host, manifest: manifest, descriptor: descriptor,
				name:     account.Name,
				scope:    sdk.Scope{Plugin: manifest.ID, Provider: descriptor.ID, Account: account.ID, Generation: account.Generation},
				modelKey: modelRevision, enableGeneration: entry.EnableGeneration}))
		}
	}
	return out, nil
}

func extensionPolicies(providers []Provider) ([]Provider, error) {
	if settings.AccountDiscoveryOff() {
		return providers, nil
	}
	loginsMu.Lock()
	defer loginsMu.Unlock()
	logins, err := readLoginsFile()
	if err != nil {
		return nil, err
	}
	known := map[string]savedLogin{}
	for _, login := range logins {
		known[login.Agent+"\x00"+login.User] = login
	}
	changed := false
	for _, provider := range providers {
		if provider.Account.extension.scope.Account == "" {
			continue
		}
		key := provider.ID + "\x00" + provider.Account.User
		if _, exists := known[key]; !exists {
			login := savedLogin{Agent: provider.ID, User: provider.Account.User, Home: provider.Account.User, On: true,
				Seen: time.Now().UTC().Truncate(time.Second)}
			logins = append(logins, login)
			known[key], changed = login, true
		}
	}
	if changed {
		if err := writeLogins(logins); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(providers, func(left, right int) bool {
		a, b := providers[left], providers[right]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		x, y := known[a.ID+"\x00"+a.Account.User].Order, known[b.ID+"\x00"+b.Account.User].Order
		return x > 0 && (y == 0 || x < y)
	})
	return providers, nil
}

func extensionAccounts() []Provider {
	providers, err := ExtensionProviders()
	if err != nil {
		log.Printf("extension providers unavailable: %v", err)
		return nil
	}
	seen := map[string]int{}
	out := []Provider{}
	for _, provider := range providers {
		if index, exists := seen[provider.ID]; !exists {
			seen[provider.ID] = len(out)
			out = append(out, provider)
		} else if !extensionAccountOn(out[index].ID, out[index].Account.User) && extensionAccountOn(provider.ID, provider.Account.User) {
			out[index] = provider
		}
	}
	for index := range out {
		primary := out[index]
		out[index].Account.models = func() []catalog.Model { return extensionServedModels(primary, providers) }
	}
	return out
}

func extensionServedModels(primary Provider, providers []Provider) []catalog.Model {
	models := primary.Account.extension.models
	out, _ := models()
	seen := map[string]bool{}
	for _, model := range out {
		seen[model.ID] = true
	}
	for _, provider := range providers {
		if provider.ID != primary.ID || provider.Account.User == primary.Account.User || !extensionAccountOn(provider.ID, provider.Account.User) {
			continue
		}
		models, err := provider.Account.extension.models()
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
	id := ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider)
	user := binding.scope.Account
	if user == "" {
		user = binding.descriptor.Name
	}
	account := &Account{Agent: id, User: user, extension: binding, magpieList: true, listedOnly: true}
	account.models = func() []catalog.Model {
		models, _ := binding.models()
		return models
	}
	account.fetch = binding.fetchModels
	account.sign = func(ctx context.Context, _ *http.Request, _ []byte) error {
		return binding.check(ctx)
	}
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
	if err := binding.checkAvailability(ctx); err != nil {
		return err
	}
	scope := binding.currentScope()
	if scope.Account != "" {
		account, err := binding.host.Store().RefreshedAccountContext(ctx, scope)
		if err != nil {
			extensionSelectionFailure(ctx, scope, err)
			return err
		}
		if binding.generation != nil {
			binding.generation.CompareAndSwap(scope.Generation, account.Generation)
		} else if account.Generation != scope.Generation {
			return errors.New("extension account generation changed")
		}
	}
	return nil
}

func (binding *extensionAccount) checkAvailability(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if binding.manifest.Contract.Has(sdk.AccountAuth) && settings.AccountDiscoveryOff() {
		return errAccountDiscoveryOff
	}
	entry, err := enabledExtensionModule(ctx, binding.host, binding.scope.Plugin)
	if err != nil {
		return err
	}
	current := entry.Manifest
	found := slices.ContainsFunc(current.Providers, func(descriptor sdk.ProviderDescriptor) bool {
		return descriptor.ID == binding.scope.Provider && descriptor.AccountOnly() == binding.descriptor.AccountOnly() &&
			(current.Contract.Has(sdk.OutboundExecutor) || descriptor.AccountOnly() && current.Contract.Has(sdk.AccountAuth))
	})
	if !found {
		return errors.New("extension provider is unavailable")
	}
	return nil
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
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s", binding.host.Namespace(), scope.Plugin, scope.Provider,
		scope.Account, scope.Generation, binding.modelKey)
}

func loginReadingCacheKey(login Login) string {
	if login.extension != nil {
		return extensionUsageKey(login.extension)
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
	cache := &usedCache
	if cache.extensions == nil {
		cache.extensions = map[string]map[string]string{}
	}
	before, known := cache.extensions[agent]
	if known && maps.Equal(before, bindings) {
		return
	}
	cache.extensions[agent] = maps.Clone(bindings)
	if cache.at != nil {
		cache.at[agent] = time.Time{}
	}
	if previous, exists := cache.m[agent]; exists {
		cache.m[agent] = maps.Clone(previous)
	}
	for user, previous := range before {
		if previous == bindings[user] {
			continue
		}
		delete(cache.m[agent], user)
		key := agent + "/" + strings.ToLower(user)
		delete(cache.read, key)
		delete(cache.seen, key)
		delete(cache.heard, key)
		delete(cache.renewed, key)
	}
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
	if err := binding.check(ctx); err != nil {
		return nil, false, err
	}
	if binding.descriptor.AccountOnly() {
		return nil, false, binding.checkModelRevision(ctx)
	}
	if !binding.manifest.Contract.Has(sdk.ModelDiscovery) {
		models, err := binding.projectModels(binding.descriptor.Models)
		return models, false, err
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return nil, false, err
	}
	key := binding.cacheKey()
	models := binding.descriptor.Models
	publish, known := false, false
	if cached, ok := extensionModelCache.Load(key); ok {
		models = cached
		known = true
	} else if cached, known, err := binding.readModelCatalog(ctx); err != nil {
		return nil, false, err
	} else if known {
		models = cached
		publish = true
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return nil, false, err
	}
	if err := binding.check(ctx); err != nil {
		return nil, false, err
	}
	if publish {
		extensionModelCache.Store(key, cloneExtensionModels(models))
		known = true
	}
	if !known {
		models = slices.DeleteFunc(slices.Clone(models), func(model sdk.Model) bool { return model.Kind == "decision" })
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
		entry := catalog.Model{ID: model.ID, Name: model.Name, Provider: ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider),
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
	if err := binding.checkModelRevision(ctx); err != nil {
		return nil, err
	}
	ctx, err = extensionNetworkContext(ctx, binding.scope)
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
	if err := binding.check(ctx); err != nil {
		return nil, err
	}
	if err := binding.writeModelCatalog(ctx, result); err != nil {
		return nil, err
	}
	extensionModelCache.Store(binding.cacheKey(), cloneExtensionModels(result.Models))
	Changed()
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
	all, err := ExtensionProviders()
	if err != nil {
		return nil
	}
	out := []Provider{}
	for _, other := range all {
		if other.ID == p.ID && other.Account.User != p.Account.User && extensionAccountOn(other.ID, other.Account.User) {
			out = append(out, other)
		}
	}
	return out
}

func extensionLogins(agent string) ([]Login, bool) {
	if !strings.HasPrefix(agent, "extension-") {
		return nil, false
	}
	return listExtensionLogins(agent), true
}

// listExtensionLogins accepts an empty agent to list every SDK account.
func listExtensionLogins(agent string) []Login {
	providers, err := ExtensionProviders()
	if err != nil {
		log.Printf("extension logins unavailable: %v", err)
		return nil
	}
	saved, err := readLoginsFile()
	if err != nil {
		log.Printf("extension logins unavailable: %v", err)
		return nil
	}
	type accountKey struct{ agent, user string }
	records := make(map[accountKey]savedLogin, len(saved))
	for _, record := range saved {
		key := accountKey{record.Agent, record.User}
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
		if record, exists := records[accountKey{login.Agent, login.User}]; exists {
			login.On = record.On && record.Hidden == ""
			login.Lapsed = extensionAuthLapse(binding, record)
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
		quota.Error, quota.ReadDeferred = err.Error(), netproxy.Held(ctx) != nil
		return quota
	}
	binding = prepared
	request, proof, err := extensionUsageRequest(binding)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), true
		return quota
	}
	quota.extensionUsage = proof
	ctx, err = extensionNetworkContext(ctx, binding.scope)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), netproxy.Held(ctx) != nil
		return quota
	}
	authRead, err := binding.beginUsageAuth(ctx)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), true
		return quota
	}
	var result sdk.UsageResult
	raw, err := binding.host.Call(ctx, binding.scope.Plugin, sdk.MethodProviderUsage, binding.scope, request, nil)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	if err != nil {
		quota.Error = err.Error()
		if extensionUsageUnauthenticated(err) {
			if publishErr := binding.finishUsageAuth(ctx, authRead, true); publishErr != nil {
				quota.ReadDeferred = true
			}
		}
		return quota
	}
	if err := binding.check(ctx); err != nil {
		quota.Error = err.Error()
		return quota
	}
	validated, fresh, err := sdk.ValidateUsageResult(binding.manifest, raw)
	if err != nil {
		quota.Error = err.Error()
		return quota
	}
	result = validated
	if proof != nil {
		proof.fresh = fresh
		if binding.manifest.Contract.Version.Minor >= 4 {
			if result.Revision != proof.stamp.Version() {
				quota.Error, quota.ReadDeferred = "usage result revision did not match its read", true
				return quota
			}
			if fresh {
				if _, err := proof.store.AcknowledgeUsage(binding.scope, proof.stamp.Revision); err != nil {
					quota.Error, quota.ReadDeferred = err.Error(), true
					return quota
				}
			}
		}
		if !proof.current() {
			quota.Error, quota.ReadDeferred = "usage read preceded an invalidation or lacks a fresh acknowledgment", true
			return quota
		}
	}
	quota.Error, quota.ReadDeferred = extensionUsageError(result.Error), result.ReadDeferred
	quota.AsOf, err = extensionUsageTime(result.AsOf)
	if err != nil {
		quota.Error = err.Error()
		return quota
	}
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
		entry.AsOf, err = extensionUsageTime(window.AsOf)
		if err == nil {
			entry.ResetsAt, err = extensionUsageTime(window.ResetsAt)
		}
		if err != nil {
			quota.Windows, quota.Error = nil, err.Error()
			return quota
		}
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

func extensionUsageTime(milliseconds int64) (*time.Time, error) {
	if milliseconds == 0 {
		return nil, nil
	}
	at := time.UnixMilli(milliseconds).UTC()
	if milliseconds < 0 || at.Year() < 1 || at.Year() > 9999 {
		return nil, errors.New("extension provider returned an invalid usage timestamp")
	}
	return &at, nil
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
	if settings.AccountDiscoveryOff() {
		return true
	}
	logins, err := readLoginsFile()
	if err != nil {
		return false
	}
	for _, saved := range logins {
		if saved.Agent == agent && saved.User == user {
			return saved.On && saved.Hidden == ""
		}
	}
	return true
}

func setExtensionLoginOn(agent, user string, on bool) error {
	logins, _ := extensionLogins(agent)
	if !slices.ContainsFunc(logins, func(login Login) bool { return login.User == user }) {
		return errors.New("extension account is unavailable")
	}
	loginsMu.Lock()
	defer loginsMu.Unlock()
	saved, err := readLoginsFile()
	if err != nil {
		return err
	}
	for index := range saved {
		if saved[index].Agent == agent && saved[index].User == user {
			saved[index].On = on
			return writeLogins(saved)
		}
	}
	return writeLogins(append(saved, savedLogin{Agent: agent, User: user, Home: user, On: on}))
}

func switchExtensionLogin(agent, user string) error {
	logins, _ := extensionLogins(agent)
	var order []string
	found := false
	for _, login := range logins {
		if login.User == user && login.On {
			found = true
		} else {
			order = append(order, login.User)
		}
	}
	if !found {
		return errors.New("the extension account must be available and in use")
	}
	return rankLogins(agent, append([]string{user}, order...))
}

func ForgetExtensionLogin(agent string, account sdk.AccountSelection) error {
	if settings.AccountDiscoveryOff() {
		return errAccountDiscoveryOff
	}
	generation, err := account.Validate()
	if err != nil {
		return err
	}
	return forgetExtensionLogin(agent, account.ID, generation)
}

func forgetExtensionLogin(agent, user string, expectedGeneration uint64) error {
	logins, _ := extensionLogins(agent)
	for _, login := range logins {
		if login.User != user {
			continue
		}
		scope := login.extension.currentScope()
		if expectedGeneration != 0 && scope.Generation != expectedGeneration {
			return errors.New("extension account generation changed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := LogoutExtensionAuth(ctx, scope)
		cancel()
		if err != nil {
			return err
		}
		loginsMu.Lock()
		defer loginsMu.Unlock()
		saved, err := readLoginsFile()
		if err != nil {
			return err
		}
		return writeLogins(slices.DeleteFunc(saved, func(login savedLogin) bool {
			return login.Agent == agent && login.User == user
		}))
	}
	return errors.New("extension account is unavailable")
}

func extensionUsageFetches(via func(string) context.Context, hidden map[string]bool) []func() SubscriptionQuota {
	out := []func() SubscriptionQuota{}
	for _, provider := range extensionAccounts() {
		if hidden[provider.ID] || leftAlone(provider.ID) || !provider.Account.extension.manifest.Contract.Has(sdk.UsageReporter) {
			continue
		}
		logins, _ := extensionLogins(provider.ID)
		ctx := via(provider.ID)
		if refresh, ok := refreshing(ctx); ok && refresh.provider != provider.ID {
			continue
		}
		out = append(out, perLogin(ctx, logins, cardFace{name: provider.Name})...)
	}
	return out
}
