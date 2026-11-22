package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionKeepAliveLookMost = 30 * time.Second

type extensionSignIn struct {
	host   extensionProviderHost
	entry  extensionhost.Installed
	config sdk.ConfigSnapshot
	scope  sdk.Scope
	public extensionPublicIdentity
	at     time.Time
}

type extensionSignInDue struct {
	namespace, revision, config string
	scope                       sdk.Scope
	at                          int64
}

func (selected extensionSignIn) key() extensionSignInDue {
	return extensionSignInDue{
		namespace: selected.host.Namespace(), revision: selected.entry.Revision + "\x00" + selected.public.revision, config: selected.config.Version,
		scope: selected.scope, at: selected.at.UnixMilli(),
	}
}

type extensionKeepAlivePolicy struct {
	providers file
	logins    []savedLogin
}

func requireExtensionAccountDiscovery(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	off, err := settings.AccountDiscoveryOffNow()
	if err != nil {
		return err
	}
	if off {
		return errAccountDiscoveryOff
	}
	return ctx.Err()
}

func extensionKeepAlivePolicyNow(ctx context.Context) (extensionKeepAlivePolicy, error) {
	if err := requireExtensionAccountDiscovery(ctx); err != nil {
		return extensionKeepAlivePolicy{}, err
	}
	return readExtensionKeepAlivePolicy(ctx)
}

func readExtensionKeepAlivePolicy(ctx context.Context) (extensionKeepAlivePolicy, error) {
	providers, err := filememo.ReadCurrent("providers", Path(), parseProviders)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return extensionKeepAlivePolicy{}, err
	}
	logins, err := readCurrentLogins()
	if err != nil {
		return extensionKeepAlivePolicy{}, err
	}
	return extensionKeepAlivePolicy{providers: providers, logins: logins}, ctx.Err()
}

func (policy extensionKeepAlivePolicy) allows(identity extensionPublicIdentity) bool {
	if !identity.visible {
		return false
	}
	for _, stored := range policy.providers.Providers {
		if stored.ID == identity.provider && (stored.Off || stored.Hidden) {
			return false
		}
	}
	for _, login := range policy.logins {
		if identity.hasPolicy(login) {
			return extensionPolicyOn(login) &&
				(login.Lapsed == "" || identity.revision != "" || validExtensionAuthMark(login.Refused))
		}
	}
	return false
}

func (policy extensionKeepAlivePolicy) allowsSelection(selected extensionSignIn, generation uint64) bool {
	identity := selected.public
	if identity.provider == "" {
		identity = heldSubscriptionOwners().identity(selected.scope)
	}
	if !policy.allows(identity) {
		return false
	}
	scope := selected.scope
	scope.Generation = generation
	binding := &extensionAccount{host: selected.host, scope: scope, public: identity,
		modelKey: selected.entry.Revision + "\x00" + selected.config.Version, enableGeneration: selected.entry.EnableGeneration}
	for _, login := range policy.logins {
		if identity.hasPolicy(login) {
			return extensionAuthLapse(binding, login) == ""
		}
	}
	return false
}

var extensionSignInsKept = func(ctx context.Context, host extensionProviderHost) ([]extensionSignIn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return nil, err
	}
	off, err := settings.AccountDiscoveryOffNow()
	if err != nil || off {
		return nil, err
	}
	entries, err := activeProviderModules(ctx, host)
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(entries, func(entry extensionhost.Installed) bool {
		return entry.Manifest.Contract.Version.Minor < 8 ||
			!entry.Manifest.Contract.Has(sdk.AccountAuth) ||
			!slices.ContainsFunc(entry.Manifest.Providers, func(provider sdk.ProviderDescriptor) bool {
				return provider.KeepAlive.Lead() != 0
			})
	})
	if len(entries) == 0 {
		return nil, nil
	}
	policy, err := extensionKeepAlivePolicyNow(ctx)
	if err != nil {
		return nil, err
	}
	var out []extensionSignIn
	owners := currentSubscriptionOwners(ctx)
	if owners.err != nil {
		return nil, owners.err
	}
	for _, entry := range entries {
		config, err := host.ModuleConfig(ctx, entry.Manifest.ID)
		if err != nil {
			return nil, err
		}
		entry.Manifest = entry.Manifest.Clone()
		descriptors := slices.DeleteFunc(slices.Clone(entry.Manifest.Providers), func(descriptor sdk.ProviderDescriptor) bool {
			if descriptor.KeepAlive.Lead() == 0 {
				return true
			}
			scope := sdk.Scope{Plugin: entry.Manifest.ID, Provider: descriptor.ID}
			identity := owners.identity(scope)
			return !slices.ContainsFunc(policy.logins, func(login savedLogin) bool {
				if login.Agent != identity.policyAgent {
					return false
				}
				candidate := identity
				candidate.user = login.User
				return policy.allows(candidate)
			})
		})
		accounts, err := extensionAccountMetadata(ctx, host, entry.Manifest.ID, descriptors, false)
		if err != nil {
			return nil, err
		}
		for _, descriptor := range descriptors {
			lead := descriptor.KeepAlive.Lead()
			scope := sdk.Scope{Plugin: entry.Manifest.ID, Provider: descriptor.ID}
			identities := map[string]bool{}
			for _, account := range accounts[descriptor.ID] {
				identity := strings.ToLower(account.ID)
				if identities[identity] {
					return nil, errors.New("extension keep-alive account identities are ambiguous")
				}
				identities[identity] = true
				if account.ExpiresAt == 0 {
					continue
				}
				scope.Account, scope.Generation = account.ID, account.Generation
				selected := extensionSignIn{
					host: host, entry: entry, config: config,
					scope: scope, public: owners.identity(scope),
					at: time.UnixMilli(account.ExpiresAt).Add(-lead),
				}
				if policy.allowsSelection(selected, account.Generation) {
					out = append(out, selected)
				}
			}
		}
	}
	return out, nil
}

func (selected extensionSignIn) current(ctx context.Context, generation uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := netproxy.Held(ctx); err != nil {
		return err
	}
	if err := CheckExtensionSubscription(ctx, selected.scope.Plugin, selected.scope.Provider, selected.public.revision); err != nil {
		return err
	}
	policy, err := extensionKeepAlivePolicyNow(ctx)
	if err != nil {
		return err
	}
	if !policy.allowsSelection(selected, generation) {
		return errors.New("extension account is not enabled and listed for idle renewal")
	}
	entry, err := enabledExtensionModule(ctx, selected.host, selected.scope.Plugin)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err != nil || entry.Revision != selected.entry.Revision || entry.EnableGeneration != selected.entry.EnableGeneration {
		return extensionhost.ErrActionChanged
	}
	config, err := selected.host.ModuleConfig(ctx, selected.scope.Plugin)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err != nil || config.Version != selected.config.Version {
		return extensionhost.ErrActionChanged
	}
	account, err := selected.host.Store().GetAccountContext(ctx, selected.scope.Plugin, selected.scope.Provider, selected.scope.Account)
	if err != nil {
		return err
	}
	if account.Generation != generation {
		return errors.New("extension keep-alive account generation changed")
	}
	return nil
}

var keepExtensionSignedIn = func(ctx context.Context, selected extensionSignIn) error {
	if selected.public.provider == "" {
		public, err := readSubscriptionIdentity(ctx, selected.scope)
		if err != nil {
			return err
		}
		selected.public = public
	}
	if err := selected.current(ctx, selected.scope.Generation); err != nil {
		return err
	}
	host, ok := selected.host.(interface {
		AcquireRevision(context.Context, string, string, string) (*extensionhost.Lease, error)
	})
	if !ok {
		return errors.New("extension keep-alive requires the selected runtime lease")
	}
	network, err := extensionNetworkContext(extensionhost.WithSubscriptionOwnerRevision(ctx, selected.public.revision), selected.scope)
	if err != nil {
		return err
	}
	owned, cancel := context.WithCancel(context.WithoutCancel(network))
	defer cancel()
	stopCaller := context.AfterFunc(ctx, cancel)
	startLimit := time.AfterFunc(keepAliveRenewWait, cancel)
	lease, err := host.AcquireRevision(owned, selected.scope.Plugin, selected.entry.Revision, selected.config.Version)
	stopCaller()
	startLimit.Stop()
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := selected.current(ctx, selected.scope.Generation); err != nil {
		return err
	}
	attempt, stop := context.WithTimeout(owned, keepAliveRenewWait)
	defer stop()
	attempt = extensionhost.WithRefreshLifecycle(attempt, ctx)
	attempt, err = extensionhost.WithRefreshSelection(attempt, selected.host.Namespace(), selected.entry, selected.config)
	if err != nil {
		return err
	}
	attempt = extensionhost.WithRefreshPolicy(attempt, func(current context.Context) error {
		policy, err := readExtensionKeepAlivePolicy(current)
		if err != nil {
			return err
		}
		if !policy.allowsSelection(selected, selected.scope.Generation) {
			return errors.New("extension account is not enabled and listed for idle renewal")
		}
		return nil
	})
	raw, err := lease.Call(attempt, sdk.MethodAuthRefresh, selected.scope, sdk.AuthRefreshRequest{}, nil)
	if err != nil {
		return err
	}
	var receipt sdk.AuthResult
	if json.Unmarshal(raw, &receipt) != nil || receipt.ID != selected.scope.Account || len(receipt.Credentials) != 0 {
		return errors.New("extension keep-alive returned no sanitized refresh receipt")
	}
	account, err := selected.host.Store().RefreshedAccount(selected.scope)
	if err != nil {
		return err
	}
	if selected.scope.Generation == ^uint64(0) || account.Generation != selected.scope.Generation+1 ||
		account.ExpiresAt != receipt.ExpiresAt || account.ExpiresAt <= time.Now().Add(30*time.Second).UnixMilli() {
		return errors.New("extension keep-alive did not prove a usable refreshed generation")
	}
	if err := selected.current(attempt, account.Generation); err != nil {
		return err
	}
	Changed()
	return nil
}

type extensionSignInKeeper struct {
	host extensionProviderHost
	held map[extensionSignInDue]keptFailure
}

func (keeper *extensionSignInKeeper) look(ctx context.Context, now time.Time) time.Duration {
	selected, err := extensionSignInsKept(ctx, keeper.host)
	if err != nil {
		return extensionKeepAliveLookMost
	}
	next := now.Add(extensionKeepAliveLookMost)
	due := map[extensionSignInDue]bool{}
	for _, account := range selected {
		if ctx.Err() != nil {
			break
		}
		key := account.key()
		due[key] = true
		if account.at.After(now) {
			next = minTime(next, account.at)
			continue
		}
		if failure, found := keeper.held[key]; found && now.Before(failure.until) {
			next = minTime(next, failure.until)
			continue
		}
		if err := keepExtensionSignedIn(ctx, account); err != nil {
			if ctx.Err() != nil {
				break
			}
			failure := keeper.held[key]
			failure.n++
			delay := keepAliveBackoff(failure.n)
			failure.until = now.Add(delay)
			if keeper.held == nil {
				keeper.held = map[extensionSignInDue]keptFailure{}
			}
			keeper.held[key] = failure
			next = minTime(next, failure.until)
			log.Printf("%s: idle SDK authentication renewal failed; retry in %s", account.scope.Plugin, delay)
		} else {
			delete(keeper.held, key)
		}
	}
	if ctx.Err() == nil {
		for key := range keeper.held {
			if !due[key] {
				delete(keeper.held, key)
			}
		}
	}
	return min(max(next.Sub(now), keepAliveLookLeast), extensionKeepAliveLookMost)
}

func KeepExtensionsSignedIn(ctx context.Context, manager *extensionhost.Manager) func() {
	if manager == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		keeper := extensionSignInKeeper{host: manager}
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				timer.Reset(keeper.look(ctx, time.Now()))
			}
		}
	}()
	return sync.OnceFunc(func() { cancel(); <-done })
}
