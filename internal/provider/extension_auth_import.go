package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func ImportSavedPluginAuth(ctx context.Context, host *extensionhost.Manager, module, providerID, account string) (extensionhost.PreparedSignIn, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := extensionhost.CheckSavedSignInDiscovery(ctx); err != nil {
		return extensionhost.PreparedSignIn{}, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return extensionhost.PreparedSignIn{}, err
	}
	if providerID == "" || account == "" || plugin.ProviderOf(account) != providerID {
		return extensionhost.PreparedSignIn{}, errors.New("select a saved sign-in of the same provider")
	}
	choice, err := savedSignInProxy(providerID, account)
	if err != nil {
		return extensionhost.PreparedSignIn{}, err
	}
	ctx = extensionhost.WithWindowAuthPolicy(netproxy.With(ctx, choice), func(ctx context.Context, _ sdk.Scope) (context.Context, error) {
		if err := extensionhost.CheckSavedSignInDiscovery(ctx); err != nil {
			return ctx, err
		}
		if err := netproxy.Held(ctx); err != nil {
			return ctx, err
		}
		current, err := savedSignInProxy(providerID, account)
		if err != nil {
			return ctx, err
		}
		if current != choice {
			return ctx, errors.New("the selected saved sign-in proxy changed")
		}
		return ctx, ctx.Err()
	})
	return host.PrepareSavedSignIn(ctx, module, providerID, account, currentSavedPluginAuth)
}

func savedSignInProxy(providerID, account string) (string, error) {
	logins, err := readCurrentLogins()
	if err != nil {
		return "", err
	}
	var user string
	for _, login := range logins {
		if login.Agent == "plugin:"+providerID && login.Home == account {
			if user != "" && accountKey(login.User) != accountKey(user) {
				return "", errors.New("the selected saved sign-in has ambiguous account settings")
			}
			user = login.User
		}
	}
	current, err := readCurrentProviders()
	if err != nil {
		return "", err
	}
	id := savedPluginID(current, providerID)
	for _, p := range current.Providers {
		if p.ID != id {
			continue
		}
		if proxy := p.AccountProxies[accountKey(user)]; proxy != "" {
			return proxy, nil
		}
		return p.Proxy, nil
	}
	return "", nil
}

func readCurrentProviders() (file, error) {
	path := Path()
	current, err := filememo.ReadCurrent("providers", path, parseProviders)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return file{}, &unreadableError{path, err}
	}
	return current, nil
}

func savedPluginID(current file, providerID string) string {
	return pluginID(providerID, func() bool { return hasCustomProvider(current, providerID) })
}

func hasCustomProvider(current file, id string) bool {
	return slices.ContainsFunc(current.Providers, func(p Provider) bool { return p.ID == id && hasEndpoint(p) })
}

func ActivateSavedPluginAuth(ctx context.Context, host *extensionhost.Manager, module, providerID, ticket string) (sdk.AccountSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := extensionhost.CheckSavedSignInDiscovery(ctx); err != nil {
		return sdk.AccountSummary{}, err
	}
	account, err := host.ActivateSavedSignIn(ctx, module, providerID, ticket,
		func(ctx context.Context, providerID, key string, use func(extensionhost.SavedSignInRecord) error) error {
			check := func() error { return extensionhost.CheckSavedSignInDiscovery(ctx) }
			return plugin.WithInstalledPlugins(ctx, check, func(installed []plugin.Entry) error {
				if err := allowSavedPluginActivation(providerID, installed); err != nil {
					return err
				}
				return currentSavedPluginAuth(ctx, providerID, key, use)
			})
		}, func(receipt sdk.AccountSummary) error {
			return registerExtensionAccountReceipt(extensionhost.WithSubscriptionOwnerRevision(ctx, ""), host,
				sdk.Scope{Plugin: module, Provider: providerID}, receipt)
		})
	if account.ID != "" {
		Changed()
	}
	return account, err
}

func allowSavedPluginActivation(providerID string, installed []plugin.Entry) error {
	if PluginServes(providerID) {
		return errSavedSignInNeedsOwnerTransfer
	}
	if len(installed) == 0 {
		return nil
	}
	listed, known := plugin.Listed()
	if !known || slices.ContainsFunc(listed, func(entry plugin.Provider) bool { return entry.ID == providerID }) {
		return errSavedSignInNeedsOwnerTransfer
	}
	return nil
}

var errSavedSignInNeedsOwnerTransfer = errors.New("this sign-in requires a subscription owner transfer to preserve routing and access rules")

func currentSavedPluginAuth(ctx context.Context, providerID, account string, use func(extensionhost.SavedSignInRecord) error) error {
	check := func() error { return extensionhost.CheckSavedSignInDiscovery(ctx) }
	if err := check(); err != nil {
		return err
	}
	return plugin.WithSavedAuthChecked(ctx, providerID, account, check, use)
}

func TransferSavedPluginSubscription(ctx context.Context, host *extensionhost.Manager, module, providerID string, tickets []string) (extensionhost.SubscriptionOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := extensionhost.CheckSavedSignInDiscovery(ctx); err != nil {
		return extensionhost.SubscriptionOwner{}, err
	}
	source, err := communitySubscription(providerID)
	if err != nil {
		return extensionhost.SubscriptionOwner{}, err
	}
	id, err := currentSavedSubscriptionID(ctx, source.ID)
	if err != nil {
		return extensionhost.SubscriptionOwner{}, err
	}
	if !CommunitySubscriptionOwnedContext(ctx, providerID) {
		pluginLoginsCheckedContext(ctx, source, func() error { return extensionhost.CheckSavedSignInDiscovery(ctx) })
	}
	return withSavedSubscriptionLogins(ctx, func() (extensionhost.SubscriptionOwner, error) {
		return host.ActivateSavedSubscription(ctx, module, providerID, id, tickets, savedSubscriptionSource(id))
	})
}

func RollbackSavedPluginSubscription(ctx context.Context, host *extensionhost.Manager, publicID, revision string) (extensionhost.SubscriptionOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := extensionhost.CheckSavedSignInDiscovery(ctx); err != nil {
		return extensionhost.SubscriptionOwner{}, err
	}
	return withSavedSubscriptionLogins(ctx, func() (extensionhost.SubscriptionOwner, error) {
		return host.RollbackSavedSubscription(ctx, publicID, revision, savedSubscriptionSource(publicID))
	})
}

func withSavedSubscriptionLogins(ctx context.Context, transition func() (extensionhost.SubscriptionOwner, error)) (extensionhost.SubscriptionOwner, error) {
	if !loginsMu.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return extensionhost.SubscriptionOwner{}, err
		}
		return extensionhost.SubscriptionOwner{}, errSubscriptionLoginsLocked
	}
	owner, err := func() (extensionhost.SubscriptionOwner, error) {
		defer loginsMu.Unlock()
		if err := extensionhost.CheckSavedSignInDiscovery(ctx); err != nil {
			return extensionhost.SubscriptionOwner{}, err
		}
		return transition()
	}()
	if owner.Revision != "" {
		Changed()
	}
	return owner, err
}

var errSubscriptionLoginsLocked = errors.New("the subscription's account settings are being changed; try again")

func communitySubscription(id string) (plugin.Provider, error) {
	var selected plugin.Provider
	for _, candidate := range plugin.Cached() {
		if candidate.ID != id {
			continue
		}
		if selected.ID != "" {
			return plugin.Provider{}, errors.New("the source subscription has ambiguous plugin ownership")
		}
		selected = candidate
	}
	if selected.ID == "" || !selected.SignedIn || publicPlugin(selected) {
		return plugin.Provider{}, errors.New("the source subscription must be installed, enabled and signed in")
	}
	return selected, nil
}

func currentSavedSubscriptionID(ctx context.Context, providerID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	current, err := readCurrentProviders()
	if err != nil {
		return "", err
	}
	id := savedPluginID(current, providerID)
	if hasCustomProvider(current, id) {
		return "", errors.New("a custom provider uses this subscription ID")
	}
	return id, ctx.Err()
}

func savedSubscriptionSource(id string) extensionhost.SavedSubscriptionSource {
	return func(ctx context.Context, providerID string, use func(map[string]json.RawMessage, []extensionhost.SubscriptionAccount, func() error) error) error {
		check := func() error { return extensionhost.CheckSavedSignInDiscovery(ctx) }
		if err := check(); err != nil {
			return err
		}
		source, err := communitySubscription(providerID)
		if err != nil {
			return err
		}
		checkID := func() error {
			current, err := currentSavedSubscriptionID(ctx, providerID)
			if err != nil {
				return err
			}
			if current != id {
				return errSubscriptionOwnerChanged
			}
			return nil
		}
		if err := checkID(); err != nil {
			return err
		}
		return plugin.WithInstalledSavedAuthsChecked(ctx, providerID, source.Spec, check, func(records map[string]extensionhost.SavedSignInRecord) error {
			current, err := communitySubscription(providerID)
			if err != nil {
				return err
			}
			if current.Spec != source.Spec {
				return errSubscriptionOwnerChanged
			}
			logins, err := filememo.ReadCurrent("logins", loginsPath(), parseLogins)
			if err != nil {
				return err
			}
			keys := slices.Sorted(maps.Keys(records))
			accounts := make([]extensionhost.SubscriptionAccount, 0, len(keys))
			raw := make(map[string]json.RawMessage, len(keys))
			for _, key := range keys {
				var user string
				for _, login := range logins {
					if login.Agent != "plugin:"+providerID || login.Home != key {
						continue
					}
					if user != "" {
						return errors.New("the source sign-in has ambiguous account settings")
					}
					user = login.User
				}
				if user == "" {
					return errors.New("a source sign-in has no public account identity")
				}
				public := savedLogin{Agent: id, User: user}
				for _, login := range logins {
					if login.Agent == id && accountKey(login.User) == accountKey(user) {
						public = login
						break
					}
				}
				accounts = append(accounts, extensionhost.SubscriptionAccount{Key: key, User: user, LoginID: loginID(public)})
				raw[key] = records[key].Raw
			}
			if err := check(); err != nil {
				return err
			}
			if err := checkID(); err != nil {
				return err
			}
			return use(raw, accounts, checkID)
		})
	}
}
