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
	denied := errors.New("scoped Memory search requires its declared local-owner tool")
	grant, _ := ctx.Value(toolMemoryGrantKey{}).(*toolMemoryGrant)
	if authority.method != sdk.MethodToolCall || grant == nil || !LocalOwner(ctx) ||
		grant.lease.manager != manager || grant.lease.closed.Load() {
		return sdk.MemorySearchResult{}, denied
	}
	request, err := sdk.DecodeMemorySearch(raw)
	if err != nil {
		return sdk.MemorySearchResult{}, err
	}
	if grant.attempts.Add(1) > 4 {
		return sdk.MemorySearchResult{}, errors.New("scoped Memory search exhausted its tool budget")
	}
	if !manager.currentToolMemoryGrant(grant) {
		return sdk.MemorySearchResult{}, denied
	}
	result, err := grant.search(ctx, request)
	if err != nil {
		return sdk.MemorySearchResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return sdk.MemorySearchResult{}, err
	}
	if !manager.currentToolMemoryGrant(grant) {
		return sdk.MemorySearchResult{}, denied
	}
	return result, nil
}

func (manager *Manager) currentToolMemoryGrant(grant *toolMemoryGrant) bool {
	selected := grant.lease.worker.entry
	entries, err := manager.List()
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Manifest.ID == selected.Manifest.ID && !entry.Disabled && entry.Revision == selected.Revision &&
			entry.observerEnablementErr == nil && entry.EnableGeneration == selected.EnableGeneration {
			config, err := manager.runtimeModuleConfig(entry.Manifest)
			return err == nil && config.Version == grant.lease.ConfigVersion()
		}
	}
	return false
}
