package provider

import (
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
