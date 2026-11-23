package provider

// A signed-in account's model list is fetched once by FetchNew, when magpie
// has none for it yet, and after that only when Refresh is pressed. A model
// the vendor adds to the plan later never reached a magpie left running, nor
// one restarted, as the list it had was not new: OpenAI served GPT-6.1 Terra
// to ChatGPT accounts on 10-26, and Codex routed through magpie still listed
// what the ChatGPT backend had on 10-12 (cliff_26651 on Discord). The magpie
// serving the gateway now asks each account's vendor again once the list it
// has is accountListAge old, as KeepPluginsListed does for a plugin's.

import (
	"context"
	"log"
	"time"
)

// accountRelistEvery is how often the gateway's magpie looks for an
// account's list that is due; accountRelistFirst is how long after it
// starts it first does (FetchNew lists the new accounts meanwhile);
// accountListAge is how old a list is before it is asked for again.
var (
	accountRelistEvery = time.Hour
	accountRelistFirst = 2 * time.Minute
	accountListAge     = 45 * time.Minute
)

// KeepAccountsListed fetches each signed-in account's list again once it
// is accountListAge old, in the magpie serving the gateway.
func KeepAccountsListed(ctx context.Context) {
	t := time.NewTimer(accountRelistFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		relistAccounts(ctx, time.Now())
		t.Reset(accountRelistEvery)
	}
}

// relistAccounts fetches the list of each account whose list is older than
// accountListAge at now, and answers the ids it asked. It also asks SDK
// accounts missing the catalog for their current selection. FetchNew asks
// for a built-in account's first list. KeepPluginsListed lists community
// plugins. A fetch that fails keeps the list there was
// (the fetch saves only what it got), and is tried at the next look.
func relistAccounts(ctx context.Context, now time.Time) []string {
	var asked []string
	for _, p := range modelRefreshProviders(AllContext(ctx)) {
		if p.Off || p.Account == nil || p.Account.fetch == nil || p.IsPlugin() || !p.Ready() {
			continue
		}
		if ctx.Err() != nil {
			return asked
		}
		if !p.IsExtension() {
			at, ok := p.Fetched()
			if !ok || now.Sub(at) < accountListAge {
				continue
			}
		}
		fctx, cancel := context.WithTimeout(ctx, time.Minute)
		var count int
		var tried bool
		var err error
		if p.IsExtension() {
			count, tried, err = p.fetchExtensionModelAccount(fctx, func(_ string, fetchedAt time.Time) bool {
				return fetchedAt.IsZero() || now.Sub(fetchedAt) >= accountListAge
			})
		} else {
			models, fetchErr := p.Fetch(fctx)
			count, tried, err = len(models), true, fetchErr
		}
		cancel()
		if tried {
			asked = append(asked, p.ID)
		}
		if err != nil {
			log.Printf("%s: fetching its list again failed: %s (the last list stays)", p.ID, err)
			continue
		}
		if tried {
			log.Printf("%s: fetched its list again, %d models", p.ID, count)
		}
	}
	return asked
}
