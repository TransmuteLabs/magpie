package extension

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type memorySearchFunc func(context.Context, sdk.MemorySearchRequest) (sdk.MemorySearchResult, error)

type toolMemoryCandidateKey struct{}
type toolMemoryGrantKey struct{}

var errToolMemoryGrant = errors.New("scoped Memory search requires its declared local-owner tool")

type toolMemoryGrant struct {
	lease    *Lease
	search   memorySearchFunc
	attempts atomic.Int32
}

func WithToolMemorySearch(ctx context.Context, search memorySearchFunc) context.Context {
	return context.WithValue(ctx, toolMemoryCandidateKey{}, search)
}

func grantToolMemory(ctx context.Context, lease *Lease, tool sdk.ToolDescriptor) context.Context {
	var grant *toolMemoryGrant
	if tool.MemorySearch && tool.Exposure == "local-owner" && LocalOwner(ctx) {
		if search, _ := ctx.Value(toolMemoryCandidateKey{}).(memorySearchFunc); search != nil {
			grant = &toolMemoryGrant{lease: lease, search: search}
		}
	}
	return context.WithValue(ctx, toolMemoryGrantKey{}, grant)
}

func (manager *Manager) searchToolMemory(ctx context.Context, raw json.RawMessage, authority hostAuthority) (sdk.MemorySearchResult, error) {
	grant, _ := ctx.Value(toolMemoryGrantKey{}).(*toolMemoryGrant)
	if authority.method != sdk.MethodToolCall || grant == nil || !LocalOwner(ctx) ||
		grant.lease.manager != manager || grant.lease.closed.Load() {
		return sdk.MemorySearchResult{}, errToolMemoryGrant
	}
	request, err := sdk.DecodeMemorySearch(raw)
	if err != nil {
		return sdk.MemorySearchResult{}, err
	}
	if grant.attempts.Add(1) > 4 {
		return sdk.MemorySearchResult{}, errors.New("scoped Memory search exhausted its tool budget")
	}
	if err := manager.checkToolMemoryGrant(ctx, grant); err != nil {
		return sdk.MemorySearchResult{}, err
	}
	result, err := grant.search(ctx, request)
	if err != nil {
		return sdk.MemorySearchResult{}, err
	}
	if err := manager.checkToolMemoryGrant(ctx, grant); err != nil {
		return sdk.MemorySearchResult{}, err
	}
	return result, nil
}

func (manager *Manager) checkToolMemoryGrant(ctx context.Context, grant *toolMemoryGrant) error {
	selected := grant.lease.worker.entry
	for _, entry := range manager.contributionEntries(ctx) {
		if entry.Manifest.ID == selected.Manifest.ID && entry.Revision == selected.Revision &&
			entry.EnableGeneration == selected.EnableGeneration {
			config, err := manager.runtimeModuleConfigContext(ctx, entry.Manifest)
			if err == nil && config.Version == grant.lease.ConfigVersion() && !grant.lease.closed.Load() {
				return ctx.Err()
			}
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errToolMemoryGrant
}
