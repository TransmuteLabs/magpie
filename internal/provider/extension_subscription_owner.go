package provider

import (
	"context"
	"errors"
	"slices"
	"strings"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionSubscriptionOwners struct {
	rows []extensionhost.SubscriptionOwner
	err  error
}

func currentSubscriptionOwners(ctx context.Context) extensionSubscriptionOwners {
	rows, err := providerExtensionHost().Store().SubscriptionOwners(ctx)
	return extensionSubscriptionOwners{rows: rows, err: err}
}

func heldSubscriptionOwners() extensionSubscriptionOwners {
	return heldSubscriptionOwnersContext(context.Background())
}

func heldSubscriptionOwnersContext(ctx context.Context) extensionSubscriptionOwners {
	owners := heldOfContext(ctx, "extension-subscription-owners", func() extensionSubscriptionOwners {
		return currentSubscriptionOwners(ctx)
	})
	if err := ctx.Err(); err != nil {
		return extensionSubscriptionOwners{err: err}
	}
	return owners
}

func (owners extensionSubscriptionOwners) byPublicID(id string) (extensionhost.SubscriptionOwner, bool) {
	for _, owner := range owners.rows {
		if owner.Active && owner.PublicID == id {
			return owner, true
		}
	}
	return extensionhost.SubscriptionOwner{}, false
}

func (owners extensionSubscriptionOwners) replaces(providerID string) bool {
	return owners.err != nil || slices.ContainsFunc(owners.rows, func(owner extensionhost.SubscriptionOwner) bool {
		return owner.Active && owner.Provider == providerID
	})
}

type extensionPublicIdentity struct {
	provider, user, policyAgent, revision string
	visible                               bool
}

func (owners extensionSubscriptionOwners) identity(scope sdk.Scope) extensionPublicIdentity {
	login := extensionhost.ResolveSubscriptionLogin(owners.rows, scope)
	policyAgent := login.Provider
	if login.Visible && login.Revision != "" {
		policyAgent = "plugin:" + scope.Provider
	}
	return extensionPublicIdentity{provider: login.Provider, user: login.User, policyAgent: policyAgent,
		revision: login.Revision, visible: owners.err == nil && login.Visible}
}

func ExtensionPublicProvider(module, providerID string) (id, ownerRevision string, visible bool) {
	identity := heldSubscriptionOwners().identity(sdk.Scope{Plugin: module, Provider: providerID})
	return identity.provider, identity.revision, identity.visible
}

func ExtensionPublicLogin(scope sdk.Scope) (id, ownerRevision, user string, visible bool) {
	identity := heldSubscriptionOwners().identity(scope)
	return identity.provider, identity.revision, identity.user, identity.visible
}

func CommunitySubscriptionOwned(providerID string) bool {
	return CommunitySubscriptionOwnedContext(context.Background(), providerID)
}

func CommunitySubscriptionOwnedContext(ctx context.Context, providerID string) bool {
	return heldSubscriptionOwnersContext(ctx).replaces(providerID)
}

func readSubscriptionIdentity(ctx context.Context, scope sdk.Scope) (extensionPublicIdentity, error) {
	owners := currentSubscriptionOwners(ctx)
	if owners.err != nil {
		return extensionPublicIdentity{}, owners.err
	}
	return owners.identity(scope), nil
}

func CheckExtensionSubscription(ctx context.Context, module, providerID, ownerRevision string) error {
	identity, err := readSubscriptionIdentity(ctx, sdk.Scope{Plugin: module, Provider: providerID})
	if err != nil {
		return err
	}
	if !identity.visible || identity.revision != ownerRevision {
		return errSubscriptionOwnerChanged
	}
	return nil
}

func SDKSubscriptionOwns(id string) bool {
	_, owned := heldSubscriptionOwners().byPublicID(id)
	return owned
}

func isExtensionLoginAgent(agent string) bool {
	return strings.HasPrefix(agent, "extension-") || SDKSubscriptionOwns(agent)
}

func extensionLoginPolicyAgent(agent string) string {
	if owner, owned := heldSubscriptionOwners().byPublicID(agent); owned {
		return "plugin:" + owner.Provider
	}
	return agent
}

func ResolveExtensionLogin(agent, user string) (sdk.Scope, bool) {
	for _, login := range Logins(agent) {
		if login.extension != nil && accountKey(login.User) == accountKey(user) {
			return login.extension.currentScope(), true
		}
	}
	return sdk.Scope{}, false
}

func (binding *extensionAccount) identity() extensionPublicIdentity {
	if binding.public.provider != "" {
		return binding.public
	}
	return heldSubscriptionOwners().identity(binding.scope)
}

func (identity extensionPublicIdentity) hasPolicy(login savedLogin) bool {
	return login.Agent == identity.policyAgent && login.User == identity.user
}

func extensionPolicyOn(login savedLogin) bool {
	return login.On && !login.Paused && login.Hidden == ""
}

var errSubscriptionOwnerChanged = errors.New("subscription owner changed; reopen its accounts and try again")
