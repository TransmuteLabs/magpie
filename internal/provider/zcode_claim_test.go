package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func claimAliasContexts() map[string]plugin.AccountContext {
	base := plugin.AccountContext{Origin: "https://zcode.z.ai", Family: "zai", UserID: "verified-user", AccountIdentity: "customer", Generation: "login-a"}
	personal, team, start := base, base, base
	personal.Scope, personal.Kind = "personal", "individual"
	team.Scope, team.Kind, team.Organization, team.Project, team.Product = "team", "team", "org", "project", "product"
	team.Family, team.AccountIdentity = "bigmodel", "linked-customer"
	start.Scope, start.Kind = "start", "start"
	return map[string]plugin.AccountContext{"zcode": personal, "zcode#team": team, "zcode#start": start}
}

func claimAliasRPC(t *testing.T, contexts map[string]plugin.AccountContext, observed bool, rawCalls *int) {
	t.Helper()
	old := zcodeAccountRPC
	zcodeAccountRPC = func(_ context.Context, _, account string, req plugin.AccountRequest) (plugin.AccountResult, error) {
		c, ok := contexts[account]
		if !ok {
			return plugin.AccountResult{}, errors.New("unknown fixture account")
		}
		switch req.Operation {
		case "context":
			return zcodeTestData(t, c, nil), nil
		case "billing.status":
			plans := []any{}
			if observed {
				plans = append(plans, map[string]any{"plan_id": "gift", "user_plan_id": "appeared", "status": "active", "ends_at": time.Now().Unix() - 1})
			}
			return zcodeTestData(t, c, map[string]any{"plans": plans}), nil
		case "marketing.feed":
			return zcodeTestData(t, c, map[string]any{"deliveries": []any{map[string]any{"banner": map[string]any{"buttons": []any{map[string]any{"action": map[string]any{"type": "claim_zcode_plan", "args": map[string]any{"plan_id": "gift"}}}}}}}}), nil
		case "billing.claim.raw":
			*rawCalls++
			return plugin.AccountResult{}, &plugin.AccountError{Kind: "network", Dispatched: true, DispatchedKnown: true}
		}
		return plugin.AccountResult{}, errors.New("unexpected fixture operation")
	}
	t.Cleanup(func() { zcodeAccountRPC = old })
}

func TestZCodeClaimRestartChild(t *testing.T) {
	if os.Getenv("MAGPIE_ZCODE_CLAIM_CHILD") != "1" {
		return
	}
	contexts := claimAliasContexts()
	for k, c := range contexts {
		c.Generation = "login-b"
		contexts[k] = c
	}
	rawCalls := 0
	claimAliasRPC(t, contexts, true, &rawCalls)
	var result WorkBuddyCheckin
	found := false
	for _, p := range pluginCheckinProviders() {
		if p.ID != "zcode" {
			continue
		}
		for _, a := range p.accounts {
			if a.key == pluginCheckinKey("zcode", "zcode#team") {
				result, found = a.do(context.Background()), true
			}
		}
	}
	if !found || result.Outcome != CheckinFailed || !strings.Contains(result.Msg, "uncertain") || rawCalls != 0 {
		t.Fatalf("scheduled check-in misrepresented observed entitlement: found=%v result=%+v posts=%d", found, result, rawCalls)
	}
	c := contexts["zcode#start"]
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	out, err := ZCodeAccountOperation(ctx, "zcode", "zcode#start", "billing.claim", map[string]string{"plan_id": "gift"})
	var response struct {
		Outcome string `json:"outcome"`
	}
	if err != nil || json.Unmarshal(out.Data, &response) != nil || response.Outcome != "observed" || rawCalls != 0 {
		t.Fatalf("expired observed entitlement was reposted or called own claim: %s posts=%d err=%v", out.Data, rawCalls, err)
	}
}

func TestZCodeSharedClaimSurvivesRestartAcrossSelections(t *testing.T) {
	contexts := claimAliasContexts()
	auth := map[string]map[string]any{}
	for key := range contexts {
		auth[key] = map[string]any{"type": "oauth", "accountId": key}
	}
	movedPlugin(t, "zcode", auth)
	rawCalls := 0
	claimAliasRPC(t, contexts, false, &rawCalls)
	c := contexts["zcode"]
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	_, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim", map[string]string{"plan_id": "gift"})
	var accountErr *plugin.AccountError
	if !errors.As(err, &accountErr) || accountErr.Kind != "claim_unconfirmed" || rawCalls != 1 {
		t.Fatalf("lost manual claim reply: posts=%d err=%v", rawCalls, err)
	}
	if result := zcodeScheduledCheckin(context.Background(), "zcode", "zcode#team"); result.Outcome != CheckinFailed || rawCalls != 1 {
		t.Fatalf("team check-in repeated unknown manual claim: %+v posts=%d", result, rawCalls)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestZCodeClaimRestartChild$", "-test.count=1")
	child.Env = append(os.Environ(), "MAGPIE_ZCODE_CLAIM_CHILD=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("restarted process: %v\n%s", err, out)
	}
	if _, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim.raw", map[string]string{"plan_id": "gift"}); err == nil || rawCalls != 1 {
		t.Fatalf("private raw claim was exposed: posts=%d err=%v", rawCalls, err)
	}
}

func TestZCodeClaimRequiresVerifiedSessionUser(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": {"type": "oauth", "accountId": "legacy"}})
	c := plugin.AccountContext{Scope: "legacy", Generation: "login", Origin: "https://zcode.z.ai", Family: "bigmodel", AccountIdentity: "customer", Kind: "legacy-key"}
	calls := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		if req.Operation == "context" {
			return zcodeTestData(t, c, nil), nil
		}
		calls++
		return plugin.AccountResult{}, errors.New("unexpected billing operation")
	})
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	if _, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim", map[string]string{"plan_id": "gift"}); err == nil || calls != 0 {
		t.Fatalf("unverified legacy identity reached billing: calls=%d err=%v", calls, err)
	}
}

func TestZCodeDefiniteClaimRefusalsCanRetry(t *testing.T) {
	contexts := claimAliasContexts()
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": {"type": "oauth", "accountId": "fixture"}})
	c := contexts["zcode"]
	for _, tc := range []struct {
		plan    string
		refusal *plugin.AccountError
	}{
		{"undispatched", &plugin.AccountError{Kind: "unavailable", DispatchedKnown: true}},
		{"refused", &plugin.AccountError{Kind: "business", Code: 1004, Dispatched: true, DispatchedKnown: true}},
	} {
		calls := 0
		zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
			switch req.Operation {
			case "context":
				return zcodeTestData(t, c, nil), nil
			case "billing.status":
				return zcodeTestData(t, c, map[string]any{"plans": []any{}}), nil
			case "billing.claim.raw":
				calls++
				if calls == 1 {
					return plugin.AccountResult{}, tc.refusal
				}
				return zcodeTestData(t, c, map[string]any{"plan": map[string]any{"plan_id": tc.plan}}), nil
			}
			return plugin.AccountResult{}, errors.New("unexpected operation")
		})
		ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
		if _, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim", map[string]string{"plan_id": tc.plan}); err == nil {
			t.Fatal("refusal reported success")
		}
		if _, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim", map[string]string{"plan_id": tc.plan}); err != nil || calls != 2 {
			t.Fatalf("definite refusal could not retry: calls=%d err=%v", calls, err)
		}
	}
}
