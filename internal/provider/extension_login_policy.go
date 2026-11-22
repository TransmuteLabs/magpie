package provider

import (
	"context"
	"errors"
	"slices"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionLoginKey struct{ agent, user string }

func extensionKey(identity extensionPublicIdentity) extensionLoginKey {
	return extensionLoginKey{identity.policyAgent, identity.user}
}

func checkExtensionLoginForExecute(ctx context.Context, identity extensionPublicIdentity) error {
	logins, err := readCurrentLogins()
	if err != nil {
		return err
	}
	if index := slices.IndexFunc(logins, identity.hasPolicy); index >= 0 && !extensionPolicyOn(logins[index]) {
		return &sdk.Error{Code: "account_paused", Message: "The selected extension account is paused.", Outcome: "not-started"}
	}
	return ctx.Err()
}

func ensureExtensionLoginPolicy(saved []savedLogin, identity extensionPublicIdentity) ([]savedLogin, int) {
	if index := slices.IndexFunc(saved, identity.hasPolicy); index >= 0 {
		return saved, index
	}
	return append(saved, savedLogin{
		Agent: identity.policyAgent, User: identity.user, Home: identity.user,
		On: true, Seen: time.Now().UTC().Truncate(time.Second),
	}), len(saved)
}

func registerExtensionLogin(ctx context.Context, host extensionProviderHost, scope sdk.Scope) error {
	if scope.Account == "" || scope.Generation == 0 {
		return errors.New("an explicit extension account generation is required")
	}
	check := func() error { return extensionhost.CheckSavedSignInDiscovery(ctx) }
	if err := check(); err != nil {
		return err
	}
	if !loginsMu.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errSubscriptionLoginsLocked
	}
	defer loginsMu.Unlock()
	owners, err := host.Store().SubscriptionOwners(ctx)
	if err != nil {
		return err
	}
	identity := (extensionSubscriptionOwners{rows: owners}).identity(scope)
	expected, bound := extensionhost.SubscriptionOwnerRevision(ctx)
	if !identity.visible || bound && identity.revision != expected {
		return errSubscriptionOwnerChanged
	}
	if err := check(); err != nil {
		return err
	}
	saved, err := readCurrentLogins()
	if err != nil {
		return err
	}
	if err := host.Store().CheckAccountGenerationContext(ctx, scope); err != nil {
		return err
	}
	updated, _ := ensureExtensionLoginPolicy(slices.Clip(saved), identity)
	if len(updated) == len(saved) {
		return check()
	}
	return writeLoginsChecked(updated, true, func() error {
		if err := check(); err != nil {
			return err
		}
		return host.Store().CheckAccountGenerationContext(ctx, scope)
	})
}

// RegisterExtensionLogin records the policy for an explicitly completed
// sign-in. Callers pass the generation the sign-in published.
func RegisterExtensionLogin(ctx context.Context, scope sdk.Scope) error {
	return registerExtensionLogin(ctx, providerExtensionHost(), scope)
}

func registerExtensionAccountReceipt(ctx context.Context, host *extensionhost.Manager, scope sdk.Scope, receipt sdk.AccountSummary) error {
	generation, err := (sdk.AccountSelection{ID: receipt.ID, ExpectedGeneration: receipt.Generation}).Validate()
	if err != nil {
		return err
	}
	scope.Account, scope.Generation = receipt.ID, generation
	return registerExtensionLogin(ctx, host, scope)
}

func editExtensionLoginPolicies(agent string, edit func([]Login, []savedLogin) error) error {
	if !loginsMu.LockChecked() {
		return errSubscriptionLoginsLocked
	}
	defer loginsMu.Unlock()
	providers, saved, err := extensionProvidersWithPolicy()
	if err != nil {
		return err
	}
	logins := extensionLoginsFrom(providers, saved, agent)
	owners := currentSubscriptionOwners(context.Background())
	if owners.err != nil {
		return owners.err
	}
	for _, login := range logins {
		binding := login.extension
		identity := binding.identity()
		if owners.identity(binding.scope) != identity {
			return errSubscriptionOwnerChanged
		}
		saved, _ = ensureExtensionLoginPolicy(saved, identity)
	}
	if err := edit(logins, saved); err != nil {
		return err
	}
	return writeLogins(saved)
}

func setExtensionLoginOn(agent, user string, on bool) error {
	return editExtensionLoginPolicies(agent, func(logins []Login, saved []savedLogin) error {
		index := slices.IndexFunc(logins, func(login Login) bool { return login.User == user })
		if index < 0 {
			return errors.New("extension account is unavailable")
		}
		record := slices.IndexFunc(saved, logins[index].extension.identity().hasPolicy)
		saved[record].On = on
		if on {
			saved[record].Paused = false
		}
		return nil
	})
}

func switchExtensionLogin(agent, user string) error {
	return editExtensionLoginPolicies(agent, func(logins []Login, saved []savedLogin) error {
		order := make([]string, 1, len(logins)+1)
		order[0] = user
		for _, login := range logins {
			if login.User != user {
				order = append(order, login.User)
			}
		}
		return rankExtensionLogins(logins, saved, order)
	})
}

func arrangeExtensionLogins(agent string, order []string) error {
	return editExtensionLoginPolicies(agent, func(logins []Login, saved []savedLogin) error {
		available := make([]string, 0, len(logins))
		for _, login := range logins {
			available = append(available, login.User)
		}
		if err := validateAccountOrder(available, order); err != nil {
			return err
		}
		return rankExtensionLogins(logins, saved, order)
	})
}

func rankExtensionLogins(logins []Login, saved []savedLogin, order []string) error {
	first := slices.IndexFunc(logins, func(login Login) bool { return login.User == order[0] && login.On })
	if first < 0 {
		return errors.New("the extension account must be available and in use")
	}
	identity := logins[first].extension.identity()
	for index := range saved {
		record := &saved[index]
		if record.Agent != identity.policyAgent {
			continue
		}
		if identity.revision != "" {
			record.First = record.User == order[0]
		}
		if rank := slices.Index(order, record.User); rank >= 0 {
			record.Order = rank + 1
		}
	}
	return nil
}
