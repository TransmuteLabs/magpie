package provider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionQuotaInformationalBalanceSurvivesRemoteCards(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	fixtureExtensionDefaults(t, host)
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage {
			t.Fatalf("unexpected method %s", method)
		}
		return json.RawMessage(`{"windows":[{"name":"5 hours","used":10,"balance":0,"unit":"ACU","span":18000}]}`), nil
	}
	current := extensionAccounts()[0]
	quota := extensionQuota(context.Background(), Login{Agent: current.ID, extension: current.Account.extension})
	if quota.Error != "" || len(quota.Windows) != 1 {
		t.Fatalf("fixture quota: %+v", quota)
	}
	now := time.Now()
	if used, _ := allowanceOf(quota.Windows, now).For("m1", now); used != 10 {
		t.Fatalf("local used=%v", used)
	}
	peer := &fakePeer{cards: ToldRouting([]SubscriptionQuota{quota})}
	remoteQuotaHome(t, peer)
	RemoteCards(context.Background())
	p, err := Find("office")
	if err != nil {
		t.Fatal(err)
	}
	remote, inner, ok := RemoteAllowance(*p, quota.Provider+"/m1", false)
	if !ok {
		t.Fatalf("no remote allowance for %s: %+v", quota.Provider, remote)
	}
	if used, _ := remote.For(inner, now); used != 10 {
		t.Fatalf("remote changed authoritative share: local=10 remote=%v windows=%+v", used, remote)
	}
}

func TestQuotaBalanceAuthorityJSONRoundTrip(t *testing.T) {
	zero, positive, negative := 0.0, 2.5, -1.0
	for _, test := range []struct {
		name          string
		balance       *float64
		informational bool
		want          float64
	}{
		{"reported zero", &zero, true, 10},
		{"reported negative", &negative, true, 10},
		{"reported positive", &positive, true, 10},
		{"absent", nil, true, 10},
		{"legacy wallet zero", &zero, false, 100},
		{"legacy wallet negative", &negative, false, 100},
		{"legacy wallet positive", &positive, false, 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			window := QuotaWindow{Name: "5 hours", Used: 10, Balance: test.balance, balanceInformational: test.informational}
			for range 3 {
				raw, err := json.Marshal(window)
				if err != nil {
					t.Fatal(err)
				}
				var restored QuotaWindow
				if err := json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				window = restored
				if (window.Balance == nil) != (test.balance == nil) || window.Balance != nil && *window.Balance != *test.balance {
					t.Fatalf("reported balance changed: %s", raw)
				}
				if used, _ := allowanceOf([]QuotaWindow{window}, time.Now()).For("model", time.Now()); used != test.want {
					t.Fatalf("balance authority changed: used=%v want=%v JSON=%s", used, test.want, raw)
				}
			}
		})
	}
}
