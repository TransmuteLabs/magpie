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
	"strings"
	"sync"
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
	Manifest(string) (sdk.Manifest, error)
	Store() *extensionhost.Store
	Call(context.Context, string, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error)
}

var providerExtensionHost = func() extensionProviderHost { return extensionhost.Default() }

var extensionModelCache sync.Map

type extensionAccount struct {
	host       extensionProviderHost
	manifest   sdk.Manifest
	descriptor sdk.ProviderDescriptor
	scope      sdk.Scope
	modelKey   string
}

func ExtensionProviderID(module, provider string) string {
	sum := sha256.Sum256([]byte(module + "\x00" + provider))
	return "extension-" + hex.EncodeToString(sum[:12])
}

func ExtensionProviders() ([]Provider, error) {
	host := providerExtensionHost()
	installed, err := host.List()
	if err != nil {
		return nil, err
	}
	out := []Provider{}
	for _, entry := range installed {
		if entry.Disabled || !entry.Manifest.Contract.Has(sdk.OutboundExecutor) {
			continue
		}
		manifest := entry.Manifest
		for _, descriptor := range manifest.Providers {
			if !manifest.Contract.Has(sdk.AccountAuth) {
				out = append(out, extensionProvider(&extensionAccount{host: host, manifest: manifest, descriptor: descriptor,
					scope: sdk.Scope{Plugin: manifest.ID, Provider: descriptor.ID}, modelKey: entry.Revision}))
				continue
			}
			if settings.AccountDiscoveryOff() {
				continue
			}
			accounts, err := host.Store().Accounts(manifest.ID, descriptor.ID)
			if err != nil {
				return nil, err
			}
			identities := map[string]bool{}
			for _, account := range accounts {
				identity := strings.ToLower(account.ID)
				if identities[identity] {
					return nil, errors.New("extension account IDs are ambiguous under core account policy")
				}
				identities[identity] = true
				out = append(out, extensionProvider(&extensionAccount{host: host, manifest: manifest, descriptor: descriptor,
					scope:    sdk.Scope{Plugin: manifest.ID, Provider: descriptor.ID, Account: account.ID, Generation: account.Generation},
					modelKey: entry.Revision}))
			}
		}
	}
	return extensionPolicies(out)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if binding.manifest.Contract.Has(sdk.AccountAuth) && settings.AccountDiscoveryOff() {
		return errAccountDiscoveryOff
	}
	current, err := binding.host.Manifest(binding.scope.Plugin)
	if err != nil {
		return err
	}
	if !current.Contract.Has(sdk.OutboundExecutor) {
		return errors.New("extension provider executor is unavailable")
	}
	found := slices.ContainsFunc(current.Providers, func(descriptor sdk.ProviderDescriptor) bool {
		return descriptor.ID == binding.scope.Provider
	})
	if !found {
		return errors.New("extension provider is unavailable")
	}
	if binding.scope.Account != "" {
		account, err := binding.host.Store().GetAccount(binding.scope.Plugin, binding.scope.Provider, binding.scope.Account)
		if err != nil {
			return err
		}
		if account.Generation != binding.scope.Generation {
			return errors.New("extension account generation changed")
		}
	}
	return nil
}

func (binding *extensionAccount) cacheKey() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s", binding.host.Namespace(), binding.scope.Plugin, binding.scope.Provider,
		binding.scope.Account, binding.scope.Generation, binding.modelKey)
}

func loginReadingCacheKey(login Login) string {
	if login.extension != nil {
		return login.extension.cacheKey()
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
	if before, known := cache.extensions[agent]; known && maps.Equal(before, bindings) {
		return
	}
	cache.extensions[agent] = maps.Clone(bindings)
	delete(cache.m, agent)
	if cache.at != nil {
		cache.at[agent] = time.Time{}
	}
	for key := range cache.read {
		if strings.HasPrefix(key, agent+"/") {
			delete(cache.read, key)
		}
	}
	for key := range cache.seen {
		if strings.HasPrefix(key, agent+"/") {
			delete(cache.seen, key)
		}
	}
	for key := range cache.heard {
		if strings.HasPrefix(key, agent+"/") {
			delete(cache.heard, key)
		}
	}
	for key := range cache.renewed {
		if strings.HasPrefix(key, agent+"/") {
			delete(cache.renewed, key)
		}
	}
}

func (binding *extensionAccount) models() ([]catalog.Model, error) {
	models := binding.descriptor.Models
	if cached, ok := extensionModelCache.Load(binding.cacheKey()); ok {
		models = cached.([]sdk.Model)
	}
	out := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if model.ContextWindow < 0 || model.MaxOutput < 0 ||
			int64(int(model.ContextWindow)) != model.ContextWindow || int64(int(model.MaxOutput)) != model.MaxOutput {
			return nil, errors.New("extension model has invalid limits")
		}
		protocols := model.Protocols
		if len(protocols) == 0 {
			protocols = binding.descriptor.Protocols
		}
		entry := catalog.Model{ID: model.ID, Name: model.Name, Provider: ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider),
			Context: catalog.Window(int(model.ContextWindow)), Output: int(model.MaxOutput), Images: model.Vision,
			Reasoning: model.Reasoning || len(model.Efforts) > 0, Efforts: slices.Clone(model.Efforts)}
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
	if err := binding.check(ctx); err != nil {
		return nil, err
	}
	if !binding.manifest.Contract.Has(sdk.ModelDiscovery) {
		return binding.models()
	}
	ctx, err := extensionNetworkContext(ctx, binding.scope)
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
	validation := binding.manifest
	validation.Providers = slices.Clone(validation.Providers)
	for index := range validation.Providers {
		if validation.Providers[index].ID == binding.scope.Provider {
			validation.Providers[index].Models = result.Models
		}
	}
	if err := validation.Validate(); err != nil {
		return nil, fmt.Errorf("invalid extension model catalog: %w", err)
	}
	if err := binding.check(ctx); err != nil {
		return nil, err
	}
	extensionModelCache.Store(binding.cacheKey(), slices.Clone(result.Models))
	Changed()
	return binding.models()
}

func (p Provider) IsExtension() bool { return p.Account != nil && p.Account.extension != nil }

func (p Provider) ExtensionScope() (sdk.Scope, bool) {
	if !p.IsExtension() {
		return sdk.Scope{}, false
	}
	return p.Account.extension.scope, true
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
	providers, err := ExtensionProviders()
	if err != nil {
		return nil, true
	}
	out := []Login{}
	for _, provider := range providers {
		if provider.ID == agent && provider.Account.extension.scope.Account != "" {
			on := extensionAccountOn(agent, provider.Account.User)
			out = append(out, Login{Agent: agent, User: provider.Account.User,
				On: on, extension: provider.Account.extension})
		}
	}
	for index := range out {
		if out[index].On {
			out[index].Active = true
			break
		}
	}
	return out, true
}

func extensionQuota(ctx context.Context, login Login) SubscriptionQuota {
	quota := SubscriptionQuota{Provider: login.Agent, User: login.User, Windows: []QuotaWindow{}}
	binding := login.extension
	if binding == nil || !binding.manifest.Contract.Has(sdk.UsageReporter) {
		quota.Error = "extension provider does not report account usage"
		return quota
	}
	quota.Name = binding.descriptor.Name
	if err := binding.check(ctx); err != nil {
		quota.Error = err.Error()
		return quota
	}
	ctx, err := extensionNetworkContext(ctx, binding.scope)
	if err != nil {
		quota.Error, quota.ReadDeferred = err.Error(), netproxy.Held(ctx) != nil
		return quota
	}
	var result sdk.UsageResult
	raw, err := binding.host.Call(ctx, binding.scope.Plugin, sdk.MethodProviderUsage, binding.scope, sdk.UsageRequest{}, nil)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	if err != nil {
		quota.Error = err.Error()
		return quota
	}
	if err := binding.check(ctx); err != nil {
		quota.Error = err.Error()
		return quota
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
		unknown := window.Unknown || len(used) == 0 || bytes.Equal(used, []byte("null")) || window.Error != ""
		if window.Name == "" || !unknown && (math.IsNaN(window.Used) || math.IsInf(window.Used, 0) || window.Used < 0 || window.Used > 100) {
			quota.Windows, quota.Error = nil, "extension provider returned an invalid quota window"
			return quota
		}
		entry := QuotaWindow{Name: window.Name, Used: window.Used, Error: extensionUsageError(window.Error)}
		entry.AsOf, err = extensionUsageTime(window.AsOf)
		if err == nil {
			entry.ResetsAt, err = extensionUsageTime(window.ResetsAt)
		}
		if err != nil {
			quota.Windows, quota.Error = nil, err.Error()
			return quota
		}
		if unknown {
			entry.Used, entry.Text, entry.Aside, entry.noShare, entry.Unknown = 0, true, true, true, true
			entry.Display = entry.Error
			if entry.Display == "" {
				entry.Display = "unknown"
			}
		}
		if entry.AsOf != nil {
			if !entry.Text {
				entry.Display = fmt.Sprintf("%g%%", entry.Used)
				entry.Text, entry.Aside, entry.noShare = true, true, true
			}
			entry.Display += " (as of " + entry.AsOf.Format(time.RFC3339Nano) + ")"
		}
		quota.Windows = append(quota.Windows, entry)
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

func forgetExtensionLogin(agent, user string) error {
	logins, _ := extensionLogins(agent)
	for _, login := range logins {
		if login.User != user {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := LogoutExtensionAuth(ctx, login.extension.scope)
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
