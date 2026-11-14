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
	config string
	scope  sdk.Scope
	at     time.Time
}

type extensionSignInDue struct {
	namespace, revision, config string
	scope                       sdk.Scope
	at                          int64
}

func (selected extensionSignIn) key() extensionSignInDue {
	return extensionSignInDue{
		namespace: selected.host.Namespace(), revision: selected.entry.Revision, config: selected.config,
		scope: selected.scope, at: selected.at.UnixMilli(),
	}
}

type extensionKeepAlivePolicy struct {
	providers file
	logins    []savedLogin
}

func extensionKeepAlivePolicyNow() (extensionKeepAlivePolicy, error) {
	set, err := settings.LoadChecked()
	if err != nil {
		return extensionKeepAlivePolicy{}, err
	}
	if settings.AccountDiscoveryOffIn(set) {
		return extensionKeepAlivePolicy{}, errAccountDiscoveryOff
	}
	providers, err := filememo.ReadNow("providers", Path(), parseProviders)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return extensionKeepAlivePolicy{}, err
	}
	logins, err := filememo.ReadNow("logins", loginsPath(), parseLogins)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return extensionKeepAlivePolicy{}, err
	}
	return extensionKeepAlivePolicy{providers: providers, logins: logins}, nil
}

func (policy extensionKeepAlivePolicy) allows(provider, account string) bool {
	for _, stored := range policy.providers.Providers {
		if stored.ID == provider && (stored.Off || stored.Hidden) {
			return false
		}
	}
	for _, login := range policy.logins {
		if login.Agent == provider && login.User == account {
			return login.On && !login.Paused && login.Hidden == "" && login.Lapsed == ""
		}
	}
	return false
}

var extensionSignInsKept = func(ctx context.Context) ([]extensionSignIn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return nil, err
	}
	set, err := settings.LoadChecked()
	if err != nil || settings.AccountDiscoveryOffIn(set) {
		return nil, err
	}
	host := providerExtensionHost()
	entries, err := host.List()
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(slices.Clone(entries), func(entry extensionhost.Installed) bool {
		return entry.Disabled || entry.Manifest.Contract.Version.Minor < 8 ||
			!entry.Manifest.Contract.Has(sdk.AccountAuth) ||
			!slices.ContainsFunc(entry.Manifest.Providers, func(provider sdk.ProviderDescriptor) bool {
				return provider.KeepAlive.Lead() != 0
			})
	})
	if len(entries) == 0 {
		return nil, nil
	}
	policy, err := extensionKeepAlivePolicyNow()
	if err != nil {
		return nil, err
	}
	var out []extensionSignIn
	for _, entry := range entries {
		config, err := host.ModuleConfig(ctx, entry.Manifest.ID)
		if err != nil {
			return nil, err
		}
		entry.Manifest = entry.Manifest.Clone()
		for _, descriptor := range entry.Manifest.Providers {
			lead := descriptor.KeepAlive.Lead()
			if lead == 0 {
				continue
			}
			provider := ExtensionProviderID(entry.Manifest.ID, descriptor.ID)
			if !slices.ContainsFunc(policy.logins, func(login savedLogin) bool {
				return login.Agent == provider && policy.allows(provider, login.User)
			}) {
				continue
			}
			accounts, err := host.Store().Accounts(entry.Manifest.ID, descriptor.ID)
			if err != nil {
				return nil, err
			}
			identities := map[string]bool{}
			for _, account := range accounts {
				identity := strings.ToLower(account.ID)
				if identities[identity] {
					return nil, errors.New("extension keep-alive account identities are ambiguous")
				}
				identities[identity] = true
				if account.ExpiresAt == 0 || !policy.allows(provider, account.ID) {
					continue
				}
				out = append(out, extensionSignIn{
					host: host, entry: entry, config: config.Version,
					scope: sdk.Scope{Plugin: entry.Manifest.ID, Provider: descriptor.ID,
						Account: account.ID, Generation: account.Generation},
					at: time.UnixMilli(account.ExpiresAt).Add(-lead),
				})
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
	policy, err := extensionKeepAlivePolicyNow()
	if err != nil {
		return err
	}
	if !policy.allows(ExtensionProviderID(selected.scope.Plugin, selected.scope.Provider), selected.scope.Account) {
		return errors.New("extension account is not enabled and listed for idle renewal")
	}
	entries, err := selected.host.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry extensionhost.Installed) bool {
		return entry.Manifest.ID == selected.scope.Plugin && !entry.Disabled && entry.Revision == selected.entry.Revision
	}) {
		return extensionhost.ErrActionChanged
	}
	config, err := selected.host.ModuleConfig(ctx, selected.scope.Plugin)
	if err != nil || config.Version != selected.config {
		return extensionhost.ErrActionChanged
	}
	account, err := selected.host.Store().GetAccount(selected.scope.Plugin, selected.scope.Provider, selected.scope.Account)
	if err != nil {
		return err
	}
	if account.Generation != generation {
		return errors.New("extension keep-alive account generation changed")
	}
	return nil
}

var keepExtensionSignedIn = func(ctx context.Context, selected extensionSignIn) error {
	if err := selected.current(ctx, selected.scope.Generation); err != nil {
		return err
	}
	host, ok := selected.host.(interface {
		AcquireRevision(context.Context, string, string, string) (*extensionhost.Lease, error)
	})
	if !ok {
		return errors.New("extension keep-alive requires the selected runtime lease")
	}
	network, err := extensionNetworkContext(ctx, selected.scope)
	if err != nil {
		return err
	}
	owned, cancel := context.WithCancel(context.WithoutCancel(network))
	defer cancel()
	stopCaller := context.AfterFunc(ctx, cancel)
	startLimit := time.AfterFunc(keepAliveRenewWait, cancel)
	lease, err := host.AcquireRevision(owned, selected.scope.Plugin, selected.entry.Revision, selected.config)
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
	held map[extensionSignInDue]keptFailure
}

func (keeper *extensionSignInKeeper) look(ctx context.Context, now time.Time) time.Duration {
	selected, err := extensionSignInsKept(ctx)
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

func KeepExtensionsSignedIn(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var keeper extensionSignInKeeper
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
