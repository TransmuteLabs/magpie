package provider

import (
	"context"
	"slices"
	"sync"

	"github.com/yetone/magpie/internal/catalog"
)

func (p Provider) withExtensionModelList() Provider {
	if !p.IsExtension() || p.Account.models == nil {
		return p
	}
	account := *p.Account
	account.models = sync.OnceValue(account.models)
	account.extensionList = nil
	p.Account = &account
	return p
}

// HoldExtensionModels returns a shallow copy of providers whose SDK model
// lists are read at most once each within a single caller attempt while ctx
// remains active. Empty or canceled reads are not cached so revocation,
// withdrawal or cancellation fails closed on the next look.
func HoldExtensionModels(ctx context.Context, providers []Provider) []Provider {
	if len(providers) == 0 {
		return providers
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := slices.Clone(providers)
	for index := range out {
		provider := out[index]
		if !provider.IsExtension() || provider.Account == nil || (provider.Account.extensionList == nil && provider.Account.models == nil) {
			continue
		}
		load := provider.Account.extensionList
		if load == nil {
			models := provider.Account.models
			load = func(context.Context) []catalog.Model { return models() }
		}
		var mu sync.Mutex
		var cached []catalog.Model
		var have bool
		account := *provider.Account
		account.extensionList = func(callCtx context.Context) []catalog.Model {
			if ctx.Err() != nil {
				return nil
			}
			if callCtx == nil {
				callCtx = ctx
			} else if callCtx.Err() != nil {
				return nil
			}
			mu.Lock()
			if have {
				models := slices.Clone(cached)
				mu.Unlock()
				return models
			}
			mu.Unlock()
			models := load(callCtx)
			if ctx.Err() != nil || callCtx.Err() != nil || len(models) == 0 {
				return models
			}
			mu.Lock()
			if !have {
				cached = slices.Clone(models)
				have = true
			}
			mu.Unlock()
			return models
		}
		account.models = func() []catalog.Model { return account.extensionList(ctx) }
		out[index].Account = &account
	}
	return out
}

// extensionListPrice preserves the executor's explicit price, including
// zero, before catalog fallbacks. Core overrides and rate policy still apply.
func (p Provider) extensionListPrice(model string) (catalog.Price, bool) {
	if !p.IsExtension() || p.Account.models == nil {
		return catalog.Price{}, false
	}
	for _, entry := range p.Account.models() {
		if entry.ID == model && entry.Price != nil {
			return *entry.Price, true
		}
	}
	return catalog.Price{}, false
}
