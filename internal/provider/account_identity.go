package provider

import (
	"context"
	"slices"
	"strings"
	"sync"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
)

// CurrentAccountIDs resolves the account references used in one selection.
// Its return value shares a current login read and an owner read per store.
// A false result means the selection's stable reference is unknown.
func CurrentAccountIDs(ctx context.Context) func(Provider) (string, bool) {
	logins := sync.OnceValues(func() ([]savedLogin, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return readCurrentLogins()
	})
	discoveryOff := sync.OnceValues(settings.AccountDiscoveryOffNow)
	var owners map[*extensionhost.Store]extensionSubscriptionOwners
	return func(p Provider) (string, bool) {
		if ctx.Err() != nil {
			return "", false
		}
		a := p.Account
		if a == nil {
			return KeyID(p.Key), true
		}
		if a.User == "" {
			return "", false
		}
		if a.extension != nil {
			binding := a.extension
			if !p.publicAccount() {
				off, err := discoveryOff()
				if off || err != nil {
					return "", false
				}
			}
			store := binding.host.Store()
			snapshot, found := owners[store]
			if !found {
				rows, err := store.SubscriptionOwners(ctx)
				snapshot = extensionSubscriptionOwners{rows: rows, err: err}
				if owners == nil {
					owners = make(map[*extensionhost.Store]extensionSubscriptionOwners)
				}
				owners[store] = snapshot
			}
			scope := binding.currentScope()
			public := snapshot.identity(scope)
			if !public.visible || public.provider != p.ID || public.revision != binding.identity().revision ||
				scope.Account != "" && accountKey(public.user) != accountKey(a.User) {
				return "", false
			}
			if public.revision != "" && scope.Account != "" {
				owner, ok := snapshot.byPublicID(p.ID)
				if !ok {
					return "", false
				}
				for _, account := range owner.Accounts {
					if account.ID == scope.Account {
						return account.LoginID, account.LoginID != ""
					}
				}
				if slices.ContainsFunc(owner.Accounts, func(account extensionhost.SubscriptionAccount) bool {
					return strings.EqualFold(account.ID, scope.Account) ||
						strings.EqualFold(account.LoginID, scope.Account) ||
						accountKey(account.User) == accountKey(scope.Account)
				}) {
					return "", false
				}
			}
			if p.publicAccount() {
				return loginID(savedLogin{Agent: p.ID, User: a.User}), true
			}
		}
		agent := a.Agent
		off, err := discoveryOff()
		if err != nil {
			return "", false
		}
		if off {
			if p.IsPlugin() && publicPlugin(*a.plugin) {
				return loginID(savedLogin{Agent: p.ID, User: a.User}), true
			}
			return "", false
		}
		rows, err := logins()
		if p.IsPlugin() {
			if err != nil || !slices.ContainsFunc(rows, func(login savedLogin) bool {
				return login.Agent == pluginAgent(*a.plugin) && login.Home == a.pluginKey &&
					accountKey(login.User) == accountKey(a.User)
			}) {
				return "", false
			}
			return currentLoginID(rows, nil, p.ID, a.User, false)
		}
		requireLogin := a.token != nil || a.Agent == ChatGPTAPIID || a.Agent == "copilot" || a.codeAssist != ""
		return currentLoginID(rows, err, agent, a.User, requireLogin)
	}
}

func currentLoginID(rows []savedLogin, err error, agent, user string, requireLogin bool) (string, bool) {
	if err != nil {
		return "", false
	}
	for _, login := range rows {
		if login.Agent == agent && accountKey(login.User) == accountKey(user) {
			return loginID(login), true
		}
	}
	if requireLogin {
		return "", false
	}
	return loginID(savedLogin{Agent: agent, User: user}), true
}
