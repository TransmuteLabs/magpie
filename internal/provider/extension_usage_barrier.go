package provider

import (
	"context"
	"fmt"
	"slices"
	"sync"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionUsageProof struct {
	host             extensionProviderHost
	stamp            extensionhost.UsageStamp
	ownerRevision    string
	modelKey         string
	enableGeneration string
	configurable     bool
	fresh            bool
}

func (proof *extensionUsageProof) current() bool {
	return proof.currentContext(context.Background())
}

func (proof *extensionUsageProof) currentContext(ctx context.Context) bool {
	return proof != nil && requireExtensionAccountDiscovery(ctx) == nil && proof.currentSelection(ctx)
}

func (proof *extensionUsageProof) currentSelection(ctx context.Context) bool {
	if proof == nil || proof.host == nil || ctx.Err() != nil {
		return false
	}
	store, scope := proof.host.Store(), proof.stamp.Scope
	owners, err := store.SubscriptionOwners(ctx)
	if err != nil {
		return false
	}
	login := extensionhost.ResolveSubscriptionLogin(owners, scope)
	if !login.Visible || login.Revision != proof.ownerRevision {
		return false
	}
	current, err := store.UsageStampContext(ctx, scope)
	if err != nil || current.Dirty || current.Revision != proof.stamp.Revision {
		return false
	}
	_, err = selectedExtensionModule(ctx, proof.host, scope.Plugin, proof.modelKey, proof.enableGeneration, proof.configurable)
	return err == nil && ctx.Err() == nil
}

func extensionUsageKey(ctx context.Context, binding *extensionAccount) string {
	key := binding.cacheKey()
	if binding.enableGeneration != "" {
		key += "\x00enable:" + binding.enableGeneration
	}
	scope := binding.currentScope()
	if scope.Account == "" {
		return key
	}
	stamp, err := binding.host.Store().UsageStampContext(ctx, scope)
	if err != nil {
		return key + "\x00usage:unavailable"
	}
	return fmt.Sprintf("%s\x00usage:%d", key, stamp.Revision)
}

func loginUsageValid(ctx context.Context, login Login, quota SubscriptionQuota) bool {
	if login.extension != nil {
		return quota.extensionUsage.currentContext(ctx)
	}
	return login.pluginUsage.matches(quota.pluginUsage)
}

func currentExtensionQuotaContext(ctx context.Context, quota SubscriptionQuota) SubscriptionQuota {
	if quota.extensionUsage != nil && !quota.extensionUsage.currentContext(ctx) {
		return deferredExtensionQuota(ctx, quota)
	}
	return quota
}

type quotaUsageSnapshots map[*extensionhost.Manager]*extensionhost.UsageSnapshot

func readQuotaUsageSnapshots(ctx context.Context, quotas []SubscriptionQuota, subscriptionsOnly bool) quotaUsageSnapshots {
	var selections map[*extensionhost.Manager][]sdk.Scope
	for _, quota := range quotas {
		if subscriptionsOnly && !localSubscription(quota) {
			continue
		}
		if proof := quota.extensionUsage; proof != nil {
			if manager, ok := proof.host.(*extensionhost.Manager); ok {
				if selections == nil {
					selections = map[*extensionhost.Manager][]sdk.Scope{}
				}
				selections[manager] = append(selections[manager], proof.stamp.Scope)
			}
		}
	}
	if len(selections) == 0 {
		return nil
	}
	snapshots := make(quotaUsageSnapshots, len(selections))
	for manager, scopes := range selections {
		snapshot, err := manager.UsageSnapshot(ctx, scopes)
		if err != nil {
			snapshots[manager] = nil
		} else {
			snapshots[manager] = &snapshot
		}
	}
	return snapshots
}

func (snapshots quotaUsageSnapshots) current(ctx context.Context, proof *extensionUsageProof) bool {
	manager, _ := proof.host.(*extensionhost.Manager)
	snapshot, selected := snapshots[manager]
	if !selected {
		return proof.currentSelection(ctx)
	}
	if snapshot == nil || ctx.Err() != nil {
		return false
	}
	scope := proof.stamp.Scope
	stamp, found := snapshot.Accounts[scope]
	if !found || stamp.Dirty || stamp.Revision != proof.stamp.Revision {
		return false
	}
	login := extensionhost.ResolveSubscriptionLogin(snapshot.Owners, scope)
	module := snapshot.Modules[scope.Plugin]
	return login.Visible && login.Revision == proof.ownerRevision &&
		module.PackageRevision+"\x00"+module.ConfigVersion == proof.modelKey &&
		module.EnableGeneration == proof.enableGeneration
}

func localSubscription(quota SubscriptionQuota) bool {
	return quota.From == "" && (quota.Kind == "" || quota.Kind == "subscription")
}

func subscriptionQuotaCheck(ctx context.Context, quotas ...SubscriptionQuota) func(SubscriptionQuota) bool {
	return subscriptionQuotaCheckWithOwners(ctx, nil, quotas...)
}

func subscriptionQuotaCheckWithOwners(ctx context.Context, selected *extensionSubscriptionOwners, quotas ...SubscriptionQuota) func(SubscriptionQuota) bool {
	discovery := sync.OnceValues(func() (bool, error) { return currentUsageDiscovery(ctx) })
	snapshot := sync.OnceValue(func() quotaUsageSnapshots {
		if off, err := discovery(); err != nil || off {
			return nil
		}
		return readQuotaUsageSnapshots(ctx, quotas, true)
	})
	owners := sync.OnceValue(func() extensionSubscriptionOwners {
		if selected != nil {
			return *selected
		}
		host := providerExtensionHost()
		if manager, ok := host.(*extensionhost.Manager); ok {
			if usage := snapshot()[manager]; usage != nil {
				return extensionSubscriptionOwners{rows: usage.Owners}
			}
		}
		rows, err := host.Store().SubscriptionOwners(ctx)
		return extensionSubscriptionOwners{rows: rows, err: err}
	})
	communityLogins := sync.OnceValue(func() func(*pluginUsageSelection) bool {
		off, err := discovery()
		if err != nil {
			return nil
		}
		if off {
			return func(selected *pluginUsageSelection) bool { return selected.Public }
		}
		logins, err := readCurrentLogins()
		if err != nil {
			return nil
		}
		epochs, err := plugin.CurrentAuthEpochs(ctx)
		if err != nil {
			return nil
		}
		return func(selected *pluginUsageSelection) bool {
			return selected.currentAuth(epochs) && selected.loginIndex(logins) >= 0
		}
	})
	return func(quota SubscriptionQuota) bool {
		if !localSubscription(quota) {
			return true
		}
		if proof := quota.extensionUsage; proof != nil {
			off, err := discovery()
			return err == nil && !off && snapshot().current(ctx, proof)
		}
		snapshot := owners()
		_, owned := snapshot.byPublicID(quota.Provider)
		if owned || snapshot.err != nil || ctx.Err() != nil {
			return false
		}
		if selected := quota.pluginUsage; selected != nil {
			current := communityLogins()
			return current != nil && current(selected) && ctx.Err() == nil
		}
		return true
	}
}

// ValidateSubscriptionQuotas defers local readings the current owner can no
// longer confirm. It reads no vendor and copies the slice only on a change.
func ValidateSubscriptionQuotas(ctx context.Context, quotas []SubscriptionQuota) []SubscriptionQuota {
	current := subscriptionQuotaCheck(ctx, quotas...)
	var out []SubscriptionQuota
	for index, quota := range quotas {
		if current(quota) {
			continue
		}
		if out == nil {
			out = slices.Clone(quotas)
		}
		out[index] = deferredExtensionQuota(ctx, quota)
	}
	if out != nil {
		return out
	}
	return quotas
}

func deferredExtensionQuota(ctx context.Context, quota SubscriptionQuota) SubscriptionQuota {
	if quota.pluginUsage != nil {
		return quota.pluginUsage.deferred(ctx, quota)
	}
	reason := "account usage was invalidated; a fresh acknowledgment is required"
	if err := ctx.Err(); err != nil {
		reason = err.Error()
	}
	return SubscriptionQuota{
		extensionUsage: quota.extensionUsage, readSeq: quota.readSeq,
		Provider: quota.Provider, User: quota.User, Name: quota.Name, Icon: quota.Icon,
		Kind: quota.Kind, From: quota.From, Via: quota.Via,
		Alias: quota.Alias, Named: quota.Named, Seat: quota.Seat,
		Error: reason, ReadDeferred: true,
	}
}

func currentExtensionQuotas(ctx context.Context, quotas []SubscriptionQuota) []SubscriptionQuota {
	allowed := sync.OnceValue(func() bool { return requireExtensionAccountDiscovery(ctx) == nil })
	snapshots := sync.OnceValue(func() quotaUsageSnapshots { return readQuotaUsageSnapshots(ctx, quotas, false) })
	for index, quota := range quotas {
		if proof := quota.extensionUsage; proof != nil && (!allowed() || !snapshots().current(ctx, proof)) {
			quotas[index] = deferredExtensionQuota(ctx, quota)
		}
	}
	return quotas
}

func extensionUsageRequest(ctx context.Context, binding *extensionAccount) (sdk.UsageRequest, *extensionUsageProof, error) {
	request := sdk.UsageRequest{}
	if binding.scope.Account == "" {
		return request, nil, nil
	}
	stamp, err := binding.host.Store().UsageStampContext(ctx, binding.scope)
	if err != nil {
		return request, nil, err
	}
	if binding.manifest.Contract.Version.Minor >= 4 {
		request.Revision = stamp.Version()
	}
	return request, &extensionUsageProof{
		host: binding.host, stamp: stamp, ownerRevision: binding.identity().revision,
		modelKey: binding.modelKey, enableGeneration: binding.enableGeneration, configurable: binding.manifest.Contract.Version.Minor >= 1,
	}, nil
}
