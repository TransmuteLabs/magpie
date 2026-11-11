package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/plugin"
)

// UsageObservation keeps Start balances and Server MCP usage separate from
// the account's authoritative Coding Plan quota windows.
type UsageObservation = plugin.UsageObservation

// ZCodeAccountChoice contains only locally available account metadata.
type ZCodeAccountChoice struct {
	Provider       string `json:"provider"`
	PluginProvider string `json:"pluginProvider"`
	Account        string `json:"account"`
	User           string `json:"user"`
	Name           string `json:"name,omitempty"`
}

// ZCodeAccountChoices re-reads local sign-ins and enabled plugins without
// refreshing models or contacting the vendor.
func ZCodeAccountChoices() ([]ZCodeAccountChoice, bool) {
	if plugin.PrepareZCode != nil {
		if err := plugin.PrepareZCode(); err != nil {
			return nil, false
		}
	}
	providers, available := plugin.PassiveProviders()
	choices := []ZCodeAccountChoice{}
	if !available {
		return choices, false
	}
	for _, pp := range providers {
		if pp.ID != "zcode" {
			continue
		}
		id := PluginID(pp.ID)
		for _, login := range pluginLogins(pp) {
			choices = append(choices, ZCodeAccountChoice{
				Provider: id, PluginProvider: pp.ID, Account: login.acct.Key,
				User: login.User, Name: AccountNameOf(id, login.User),
			})
		}
	}
	return choices, true
}

func zcodePlugin(providerID string) (plugin.Provider, bool) {
	providers, available := plugin.PassiveProviders()
	if available {
		for _, pp := range providers {
			if pp.ID == "zcode" && PluginID(pp.ID) == providerID {
				return pp, true
			}
		}
	}
	return plugin.Provider{}, false
}

// zcodeSelectorHeader mirrors the execution panel: ASCII is sent as-is;
// a non-ASCII value is encoded with encodeURIComponent.
func zcodeSelectorHeader(selector string) string {
	nonASCII := false
	for i := 0; i < len(selector); i++ {
		if selector[i] >= 0x80 {
			nonASCII = true
			break
		}
	}
	if !nonASCII {
		return selector
	}
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(selector))
	for i := 0; i < len(selector); i++ {
		b := selector[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
			strings.ContainsRune("-_.!~*'()", rune(b)) {
			out.WriteByte(b)
		} else {
			out.WriteByte('%')
			out.WriteByte(hex[b>>4])
			out.WriteByte(hex[b&15])
		}
	}
	return out.String()
}

// zcodeAccountSelector names an actual routing user or trace ID, never an
// auth slot. It applies the gateway's raw-or-decoded pin matching to every
// enabled account before offering a header for this account.
func zcodeAccountSelector(providerID, accountKey string) string {
	pp, ok := zcodePlugin(providerID)
	if !ok {
		return ""
	}
	logins := pluginLogins(pp)
	selected := -1
	for i, login := range logins {
		if login.acct.Key == accountKey {
			if login.Paused || !login.Active && !login.On {
				return ""
			}
			selected = i
			break
		}
	}
	if selected < 0 {
		return ""
	}
	// accountCandidate uses the provider ID for its primary account and
	// provider@user for each additional account's Weighed.ID.
	traceID := func(login pluginLogin) string {
		if login.Active {
			return providerID
		}
		return providerID + "@" + login.User
	}
	for _, selector := range []string{logins[selected].User, traceID(logins[selected])} {
		if selector == "" || strings.TrimSpace(selector) != selector || !utf8.ValidString(selector) {
			continue
		}
		unsafe := false
		for _, r := range selector {
			if r < 0x20 || r == 0x7f {
				unsafe = true
				break
			}
		}
		if unsafe {
			continue
		}
		raw := zcodeSelectorHeader(selector)
		decoded := raw
		if strings.Contains(raw, "%") {
			if unescaped, err := url.PathUnescape(raw); err == nil && utf8.ValidString(unescaped) {
				decoded = strings.TrimSpace(unescaped)
			}
		}
		matches := 0
		selectedMatches := false
		for i, login := range logins {
			if !login.Active && !login.On {
				continue
			}
			id := traceID(login)
			if strings.EqualFold(decoded, login.User) || decoded == id || strings.EqualFold(raw, login.User) || raw == id {
				matches++
				selectedMatches = selectedMatches || i == selected
			}
		}
		if matches == 1 && selectedMatches {
			return selector
		}
	}
	return ""
}

// zcodeAccountKey resolves a named account without falling back to the
// provider's first account. An exact slot always wins over a display name.
func zcodeAccountKey(providerID, account string) (string, error) {
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(account) == "" {
		return "", errors.New("name both the provider and the account")
	}
	pp, ok := zcodePlugin(providerID)
	if !ok || pp.ID != "zcode" {
		return "", fmt.Errorf("%s is not a native ZCode account provider", providerID)
	}
	for _, a := range pp.Accounts {
		if a.Key == account {
			return a.Key, nil
		}
	}
	match := ""
	for _, a := range pluginLogins(pp) {
		if strings.EqualFold(a.User, account) {
			if match != "" && match != a.acct.Key {
				return "", errors.New("account name is ambiguous; specify its exact account key")
			}
			match = a.acct.Key
		}
	}
	if match == "" {
		return "", fmt.Errorf("no account %q on %s", account, providerID)
	}
	return match, nil
}

func zcodeLogin(providerID, accountKey string) (Login, bool) {
	pp, ok := zcodePlugin(providerID)
	if !ok {
		return Login{}, false
	}
	for _, a := range pluginLogins(pp) {
		if a.acct.Key == accountKey {
			return a.Login, true
		}
	}
	return Login{}, false
}

// ZCodeAccountOperation performs only named, nonsecret native operations.
// Reset use and native admissions have dedicated owners and cannot be
// reached by invoking their underlying plugin operations here.
func ZCodeAccountOperation(ctx context.Context, providerID, account, operation string, params any) (plugin.AccountResult, error) {
	switch operation {
	case "context", "selection.list", "selection.select", "usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.health", "usage.mcp", "usage.balance", "entitlement", "billing.preview", "billing.claim", "marketing.feed", "marketing.action", "offpeak.availability", "offpeak.status", "highspeed.healthy", "execution.take", "execution.draw", "execution.continue", "execution.status", "execution.list", "execution.finish", "execution.cancel", "execution.resolve":
	default:
		return plugin.AccountResult{}, fmt.Errorf("native account operation %q is not publicly available", operation)
	}
	switch operation {
	case "selection.select", "billing.claim", "marketing.action", "execution.take", "execution.draw", "execution.continue", "execution.finish", "execution.cancel", "execution.resolve":
		expectation, ok := expectedZCodeAccount(ctx)
		if !ok || expectation.scope == "" || expectation.generation == "" {
			return plugin.AccountResult{}, errors.New("capture the ZCode account scope and generation before confirming a change")
		}
	}
	key, err := zcodeAccountKey(providerID, account)
	if err != nil {
		return plugin.AccountResult{}, err
	}
	c, err := zcodeContext(ctx, providerID, key)
	if err != nil {
		return plugin.AccountResult{}, err
	}
	result := plugin.AccountResult{Context: c}
	switch operation {
	case "context":
		result.Data, err = json.Marshal(struct {
			AccountSelector string `json:"accountSelector,omitempty"`
		}{AccountSelector: zcodeAccountSelector(providerID, key)})
		return result, err
	case "execution.list":
		result.Data, err = zcodeExecutionList(c)
		return result, err
	case "billing.claim":
		result.Data, err = zcodeClaim(ctx, providerID, key, c, params)
		return result, err
	case "selection.list", "usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.health", "usage.mcp", "usage.balance", "entitlement", "billing.preview", "marketing.feed", "marketing.action", "offpeak.availability", "offpeak.status", "highspeed.healthy":
		result.Data, err = zcodeAccountCall(ctx, providerID, key, c, operation, params)
		return result, err
	case "selection.select":
		selected, err := zcodeAccountRPC(ctx, providerID, key, plugin.AccountRequest{Operation: operation, Params: params, Scope: c.Scope, Generation: c.Generation})
		if err != nil {
			return selected, err
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(selected.Data, &data) == nil && data != nil {
			var account string
			_ = json.Unmarshal(data["account"], &account)
			user := ""
			if account != "" {
				if login, ok := zcodeLogin(providerID, account); ok {
					user = login.User
				}
			}
			data["user"], _ = json.Marshal(user)
			selected.Data, err = json.Marshal(data)
		}
		return selected, err
	case "execution.take", "execution.draw", "execution.continue", "execution.status", "execution.finish", "execution.cancel", "execution.resolve":
		p, err := executionParams(params)
		if err != nil {
			return result, err
		}
		switch operation {
		case "execution.take":
			result.Data, err = zcodeExecutionStart(ctx, providerID, key, c, p, "offpeak", false)
		case "execution.draw":
			result.Data, err = zcodeExecutionStart(ctx, providerID, key, c, p, "highspeed", false)
		case "execution.continue":
			result.Data, err = zcodeExecutionStart(ctx, providerID, key, c, p, "offpeak", true)
		case "execution.status":
			result.Data, err = zcodeExecutionStatus(ctx, providerID, key, c, p)
		case "execution.finish", "execution.cancel":
			result.Data, err = zcodeExecutionFinish(ctx, providerID, key, c, p)
		case "execution.resolve":
			if p.Resolution != "forget" {
				return result, errors.New("explicit resolution=forget is required; the vendor may already have admitted or settled this task")
			}
			unlock, err := zcodeFence(c, "execution")
			if err != nil {
				return result, err
			}
			defer unlock()
			err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
				execution, ok := j.Executions[p.TaskID]
				if !ok {
					return false, errors.New("no execution for this task")
				}
				if execution.Generation == c.Generation && execution.Claim == "" {
					return false, errors.New("this execution is known; finish or cancel it instead")
				}
				delete(j.Executions, p.TaskID)
				return true, nil
			})
			if err == nil {
				result.Data = json.RawMessage(`{"resolved":true}`)
			}
		}
		return result, err
	default:
		return result, fmt.Errorf("native account operation %q is not publicly available", operation)
	}
}
