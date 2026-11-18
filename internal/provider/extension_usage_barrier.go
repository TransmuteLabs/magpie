package provider

import (
	"fmt"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionUsageProof struct {
	store *extensionhost.Store
	stamp extensionhost.UsageStamp
	fresh bool
}

func (proof *extensionUsageProof) current() bool {
	if proof == nil {
		return false
	}
	current, err := proof.store.UsageStamp(proof.stamp.Scope)
	return err == nil && !current.Dirty && current.Revision == proof.stamp.Revision
}

func extensionUsageKey(binding *extensionAccount) string {
	key := binding.cacheKey()
	scope := binding.currentScope()
	if scope.Account == "" {
		return key
	}
	stamp, err := binding.host.Store().UsageStamp(scope)
	if err != nil {
		return key + "\x00usage:unavailable"
	}
	return fmt.Sprintf("%s\x00usage:%d", key, stamp.Revision)
}

func extensionUsageValid(login Login, quota SubscriptionQuota) bool {
	return login.extension == nil || quota.extensionUsage != nil && quota.extensionUsage.current()
}

func currentExtensionQuota(quota SubscriptionQuota) SubscriptionQuota {
	if quota.extensionUsage != nil && !quota.extensionUsage.current() {
		quota.Windows, quota.Balance, quota.ReadAt, quota.AsOf = nil, "", nil, nil
		quota.Error, quota.ReadDeferred = "account usage was invalidated; a fresh acknowledgment is required", true
	}
	return quota
}

func currentExtensionQuotas(quotas []SubscriptionQuota) []SubscriptionQuota {
	for index := range quotas {
		quotas[index] = currentExtensionQuota(quotas[index])
	}
	return quotas
}

func extensionUsageRequest(binding *extensionAccount) (sdk.UsageRequest, *extensionUsageProof, error) {
	request := sdk.UsageRequest{}
	if binding.scope.Account == "" {
		return request, nil, nil
	}
	stamp, err := binding.host.Store().UsageStamp(binding.scope)
	if err != nil {
		return request, nil, err
	}
	if binding.manifest.Contract.Version.Minor >= 4 {
		request.Revision = stamp.Version()
	}
	return request, &extensionUsageProof{store: binding.host.Store(), stamp: stamp}, nil
}
