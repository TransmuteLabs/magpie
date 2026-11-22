package provider

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func fixtureExtensionDefaults(t *testing.T, host *fixtureExtensionProviderHost) {
	t.Helper()
	host.manifest.Contract.Version.Minor = 9
	host.manifest.Contract.Capabilities = slices.DeleteFunc(host.manifest.Contract.Capabilities, func(capability sdk.Capability) bool {
		return capability == sdk.ModelDiscovery
	})
	if err := json.Unmarshal([]byte(`{"maxConcurrency":3,"maxAccounts":2}`), &host.manifest.Providers[0]); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionDefaultsReachOrdinaryLimitsAndUserOverrides(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	fixtureExtensionDefaults(t, host)
	current := extensionAccounts()[0]
	if current.Concurrency() != 3 || current.LaneLimit() != 3 || current.AtOnce() != 2 ||
		current.PluginConcurrency() != 3 || current.PluginAtOnce() != 2 {
		t.Fatalf("SDK defaults did not reach ordinary owners: concurrency=%d lane=%d accounts=%d",
			current.Concurrency(), current.LaneLimit(), current.AtOnce())
	}
	zero, five := 0, 5
	current.MaxConcurrency, current.MaxAccounts = &zero, &zero
	if current.LaneLimit() != 0 || current.AtOnce() != 0 {
		t.Fatal("SDK defaults overrode explicit user zero")
	}
	current.MaxConcurrency, current.MaxAccounts = &five, &five
	current.AccountConcurrency = map[string]int{"alice": 0}
	if current.LaneLimit() != 0 || current.Concurrency() != 5 || current.AtOnce() != 5 {
		t.Fatal("account zero or provider override lost precedence")
	}
}

func TestExtensionDefaultsRejectStaleSelection(t *testing.T) {
	for _, change := range []string{"disabled", "config", "package", "account", "discovery"} {
		t.Run(change, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			fixtureExtensionDefaults(t, host)
			current := extensionAccounts()[0]
			if current.Concurrency() != 3 {
				t.Fatal("initial SDK default missing")
			}
			switch change {
			case "disabled":
				host.disabled = true
			case "config":
				host.config = "changed"
			case "package":
				json.Unmarshal([]byte(`{"maxConcurrency":4}`), &host.manifest.Providers[0])
			case "account":
				scope, _ := current.ExtensionScope()
				if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: scope.Plugin, Provider: scope.Provider,
					ID: scope.Account, Auth: json.RawMessage(`{"access":"replacement"}`)}, scope.Generation); err != nil {
					t.Fatal(err)
				}
			case "discovery":
				t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
			}
			if current.PluginConcurrency() != 0 || current.PluginAtOnce() != 0 {
				t.Fatal("stale binding kept provider default authority")
			}
			if change == "config" || change == "package" || change == "account" {
				fresh := extensionAccounts()[0]
				want := 3
				if change == "package" {
					want = 4
				}
				if fresh.Concurrency() != want || fresh.AtOnce() != 2 {
					t.Fatalf("replacement selection did not recover defaults: %d/%d", fresh.Concurrency(), fresh.AtOnce())
				}
			}
		})
	}
}

func TestExtensionUsageTimeKeepsUTC(t *testing.T) {
	at := extensionUsageTime(1000)
	if at == nil {
		t.Fatal("usage timestamp missing")
	}
	if at.Location() != time.UTC || at.UnixMilli() != 1000 || at.Format(time.RFC3339Nano) != "1970-01-01T00:00:01Z" {
		t.Fatalf("usage timestamp lost its UTC identity: %v", at)
	}
	t.Logf("local epoch=%s; SDK epoch=%s", time.UnixMilli(1000).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
}

func TestExtensionRichQuotaProjectsWithoutInventingAllowance(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	fixtureExtensionDefaults(t, host)
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage {
			t.Fatalf("unexpected method %s", method)
		}
		return json.RawMessage(`{"windows":[
			{"name":"Credits","used":25,"amount":2.5,"limit":10,"unit":"ACU","span":18000,"resetsAt":2000000000000},
			{"name":"Exact zero","used":0,"amount":0,"limit":10},
			{"name":"Missing amount","used":10,"limit":10},
			{"name":"Wallet","balance":0,"unit":"points","display":"Vendor wallet"},
			{"name":"Informational","amount":2,"limit":10,"display":"Gift pending","aside":true},
			{"name":"Historical","used":100,"amount":10,"limit":10,"display":"10 / 10","asOf":1000},
			{"name":"Reported share and wallet","used":10,"balance":0,"unit":"ACU"}
		]}`), nil
	}
	current := extensionAccounts()[0]
	quota := extensionQuota(context.Background(), Login{Agent: current.ID, extension: current.Account.extension})
	if quota.Error != "" || len(quota.Windows) != 7 {
		t.Fatalf("rich quota read: %+v", quota)
	}
	first := quota.Windows[0]
	if first.Amount != 2.5 || first.Limit != 10 || first.Unit != "ACU" || first.Span != 5*time.Hour ||
		first.ResetsAt == nil || first.ResetsAt.UnixMilli() != 2000000000000 {
		t.Fatalf("rich quota metadata lost: %+v", first)
	}
	display := WithWindowStarts([]SubscriptionQuota{quota}, first.ResetsAt.Add(-time.Hour))
	if display[0].Windows[0].StartsAt == nil ||
		!display[0].Windows[0].StartsAt.Equal(first.ResetsAt.Add(-5*time.Hour)) ||
		quota.Windows[0].StartsAt != nil {
		t.Fatal("vendor span/reset did not reach ordinary pace presentation without mutating the cached reading")
	}
	wallet, gift, historical := quota.Windows[3], quota.Windows[4], quota.Windows[5]
	if wallet.Balance == nil || *wallet.Balance != 0 || !wallet.Unknown || !wallet.noShare || !wallet.Aside ||
		!gift.Unknown || !gift.Text || gift.Display != "Gift pending" || !historical.noShare || !historical.Text {
		t.Fatalf("metadata invented a routing share: wallet=%+v gift=%+v historical=%+v", wallet, gift, historical)
	}
	if historical.Display != "10 / 10 (as of "+time.UnixMilli(1000).UTC().Format(time.RFC3339Nano)+")" {
		t.Fatalf("historical reading discarded vendor count text: %+v", historical)
	}
	if quota.Windows[2].Count(false) != "" {
		t.Fatal("missing amount invented a native count")
	}
	for _, value := range []any{quota.Windows, quotaReport(t.Context(), []SubscriptionQuota{quota}, nil, nil, time.Now())[0].Windows} {
		wire, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var windows []map[string]any
		json.Unmarshal(wire, &windows)
		if windows[1]["amount"] != float64(0) {
			t.Fatalf("exact zero disappeared on ordinary card/report: %s", wire)
		}
		if _, exists := windows[2]["amount"]; exists {
			t.Fatalf("missing amount invented on ordinary card/report: %s", wire)
		}
		if windows[3]["balance"] != float64(0) {
			t.Fatalf("wallet zero disappeared: %s", wire)
		}
	}
	allowance := allowanceOf(quota.Windows[2:], time.Now())
	if len(allowance) != 2 || allowance[0].Used != 10 || allowance[1].Used != 10 || allowance[1].Balance != nil {
		t.Fatalf("metadata altered ordinary quota authority: %+v", allowance)
	}
}

func TestExtensionRichQuotaCountPresenceSurvivesSerialization(t *testing.T) {
	for _, raw := range []string{
		`{"name":"absent","used":10,"limit":10}`,
		`{"name":"zero","used":0,"amount":0,"limit":10}`,
		`{"name":"fraction","used":25,"amount":2.5,"limit":10,"unit":"ACU"}`,
	} {
		var window QuotaWindow
		if err := json.Unmarshal([]byte(raw), &window); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			body, err := json.Marshal(window)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			json.Unmarshal(body, &fields)
			_, hasAmount := fields["amount"]
			if hasAmount != (window.Name != "absent") {
				t.Fatalf("quota count presence changed: %s", body)
			}
			if err := json.Unmarshal(body, &window); err != nil {
				t.Fatal(err)
			}
		}
	}
	window := QuotaWindow{Name: "legacy zero", Used: 0, Limit: 10}
	raw, _ := json.Marshal(window)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if fields["amount"] != float64(0) || window.Count(true) != "10 / 10" {
		t.Fatalf("legacy zero compatibility lost: %s", raw)
	}
}
