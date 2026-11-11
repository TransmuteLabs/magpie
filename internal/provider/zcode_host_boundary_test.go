package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	fixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
)

func TestZCodeResetAcrossPluginHost(t *testing.T) {
	claudeHome(t)
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := fixture.Install(t, ctx)
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ZCodeResetStatus(ctx, PluginID("zcode"), account, true)
	if err != nil || !state.Available || len(state.FiveHour) != 1 {
		t.Fatalf("status=%+v err=%v", state, err)
	}
	card := pluginLoginQuota(ctx, Login{Agent: "plugin:zcode", User: PluginAccountUser("zcode", account)})
	resets := card.Resets
	if card.Error != "" || card.ZCodeReset == nil || len(card.ZCodeReset.FiveHour) != 1 ||
		resets == nil || resets.Source != "zcode" || !resets.ByWindow ||
		resets.Count != 2 || resets.FiveHour != 1 || resets.Weekly != 1 || len(resets.Each) != 2 ||
		resets.Each[0].Window != "fiveHour" || resets.Each[1].Window != "weekly" || resets.Until == nil ||
		resets.Until.UnixMilli() != int64(card.ZCodeReset.FiveHour[0]) {
		t.Fatalf("native inventory is missing from the standard allowance card: error=%s state=%+v resets=%+v", card.Error, card.ZCodeReset, resets)
	}
	fenced := WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	state, err = UseZCodeReset(fenced, PluginID("zcode"), account, "five-hour")
	if err != nil || state.Pending != "" || state.Last == nil {
		t.Fatalf("use=%+v err=%v", state, err)
	}
	uses := vendor.Calls("/api/v1/coding-plan/reset/use")
	opportunities := vendor.Calls("/api/v1/coding-plan/reset/opportunity")
	acks := vendor.Calls("/api/v1/coding-plan/reset/history/read")
	if len(uses) != 1 || len(opportunities) != 1 || len(acks) != 1 {
		t.Fatalf("uses=%+v opportunities=%+v acks=%+v", uses, opportunities, acks)
	}
	var request struct {
		Key  string `json:"idempotency_key"`
		Type string `json:"reset_type"`
	}
	if json.Unmarshal(uses[0].Body, &request) != nil || request.Key == "" || request.Type != "FIVE_HOUR" ||
		uses[0].Auth != "Bearer fixture-zcode" || uses[0].Business != "fixture-business" || uses[0].Target != "PERSONAL" {
		t.Fatalf("reset vendor request = %+v", uses[0])
	}
	if acks[0].Target != "" || len(acks[0].Body) != 0 {
		t.Fatalf("history read targeted a plan: %+v", acks[0])
	}
}

func TestZCodeLegacyLostReplyAcrossRealHostCutover(t *testing.T) {
	claudeHome(t)
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := fixture.Install(t, ctx)
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldScope, _ := json.Marshal([]string{"https://zcode.z.ai", "zai", "fixture-user", "fixture-user", "", ""})
	hash := sha256.Sum256(oldScope)
	started := time.Now().UTC().Add(-time.Minute)
	old := legacyZCodeReset{Seen: true, Pending: &legacyZCodePending{
		Window: "fiveHour", Key: "old-use-key", Started: started, Claim: "old-claim", ClaimedAt: started,
	}}
	ledger, _ := json.Marshal(map[string]any{"version": 1, "scopes": map[string]any{hex.EncodeToString(hash[:]): old}})
	if err := os.WriteFile(legacyZCodeResetPath(), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	vendor.LoseResetReply(true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("ZCODE_FIXTURE_URL")+"/api/v1/coding-plan/reset/use",
		strings.NewReader(`{"idempotency_key":"old-use-key","reset_type":"FIVE_HOUR"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Fake-Run", os.Getenv("FAKE_RUN"))
	req.Header.Set("Authorization", "Bearer fixture-zcode")
	req.Header.Set("X-Bigmodel-Authorization", "fixture-business")
	req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	response, err := http.DefaultClient.Do(req)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || len(vendor.Calls("/api/v1/coding-plan/reset/use")) != 1 {
		t.Fatalf("fixture did not lose exactly one old reply: %v", err)
	}
	fenced := WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	state, err := ZCodeResetStatus(fenced, PluginID("zcode"), account, true)
	if err != nil || state.Pending != "observed-completion" || state.ObservedAt == 0 {
		t.Fatalf("imported old reply=%+v err=%v", state, err)
	}
	if _, err := UseZCodeReset(fenced, PluginID("zcode"), account, "five-hour"); err == nil {
		t.Fatal("unknown old debit was spent again")
	}
	plugin.Restart()
	var imported struct {
		Account string `json:"account"`
	}
	if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": account,
		"auth": fixture.Auth("fixture-business-new", "fixture-zcode-new")}, &imported); err != nil || imported.Account != account {
		t.Fatalf("relogin=%+v err=%v", imported, err)
	}
	latest, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	relogin := WithZCodeAccountExpectation(ctx, latest.Context.Scope, latest.Context.Generation)
	state, err = ZCodeResetStatus(relogin, PluginID("zcode"), account, true)
	if err != nil || state.Pending != "observed-completion" {
		t.Fatalf("relogin lost old debt=%+v err=%v", state, err)
	}
	if _, err := UseZCodeReset(relogin, PluginID("zcode"), account, "five-hour"); err == nil {
		t.Fatal("relogin repeated the old debit")
	}
	if calls := vendor.Calls("/api/v1/coding-plan/reset/use"); len(calls) != 1 {
		t.Fatalf("cutover caused %d debits", len(calls))
	}
}

func TestZCodeResetStaleCompletionKeepsBarrierWithoutRefresh(t *testing.T) {
	claudeHome(t)
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := fixture.Install(t, ctx)
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	login, ok := zcodeLogin(PluginID("zcode"), account)
	if !ok {
		t.Fatal("fixture login missing")
	}
	cacheKey := "plugin:zcode/" + strings.ToLower(login.User)
	loginUsageCache.Lock()
	if loginUsageCache.m == nil {
		loginUsageCache.m = map[string]loginUsageEntry{}
	}
	loginUsageCache.m[cacheKey] = loginUsageEntry{at: time.Now(), q: SubscriptionQuota{Error: "old reading"}}
	loginUsageCache.Unlock()
	vendor.OnResetAck(func() {
		b, err := os.ReadFile(plugin.AuthPath())
		var auth map[string]map[string]any
		if err == nil {
			err = json.Unmarshal(b, &auth)
		}
		if err == nil && auth[account] != nil {
			auth[account]["accountEpoch"] = "replacement-epoch"
			b, err = json.Marshal(auth)
		}
		if err == nil {
			err = os.WriteFile(plugin.AuthPath(), b, 0o600)
		}
		if err != nil {
			t.Errorf("replacing account at acknowledgement: %v", err)
		}
	})
	before := len(vendor.Calls("/api/monitor/usage/quota/limit"))
	fenced := WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	state, err := UseZCodeReset(fenced, PluginID("zcode"), account, "five-hour")
	if err != nil || state.Pending != "" || state.Last == nil {
		t.Fatalf("completion=%+v err=%v", state, err)
	}
	card := PluginID("zcode") + "/" + strings.ToLower(login.User)
	quotaReadBarriers.Lock()
	barrier := quotaReadBarriers.m[card]
	quotaReadBarriers.Unlock()
	loginUsageCache.Lock()
	generation := loginUsageCache.generation[cacheKey]
	loginUsageCache.Unlock()
	if barrier == 0 || generation == 0 || len(vendor.Calls("/api/monitor/usage/quota/limit")) != before {
		t.Fatalf("stale completion failed to invalidate only: barrier=%d generation=%d", barrier, generation)
	}
}

func TestZCodeColdHostAndStaleFenceNeverContactVendor(t *testing.T) {
	claudeHome(t)
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := fixture.Install(t, ctx)
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	var imported struct {
		Account string `json:"account"`
	}
	if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": account, "auth": fixture.Auth("fixture-business-new", "fixture-zcode-new")}, &imported); err != nil || imported.Account != account {
		t.Fatalf("relogin = %+v, %v", imported, err)
	}
	plugin.Refreshed()
	plugin.Restart()
	fenced := WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	if _, err := ZCodeAccountOperation(fenced, PluginID("zcode"), account, "execution.draw", map[string]string{"taskId": "chat", "model": "GLM-5.3"}); err == nil {
		t.Fatal("stale draw was accepted")
	}
	if _, err := UseZCodeReset(fenced, PluginID("zcode"), account, "week"); err == nil {
		t.Fatal("stale reset was accepted")
	}
	for _, path := range []string{"/api/v1/highspeed/draw", "/api/v1/coding-plan/reset/status", "/api/v1/coding-plan/reset/use"} {
		if calls := vendor.Calls(path); len(calls) != 0 {
			t.Fatalf("stale operation contacted %s: %+v", path, calls)
		}
	}
}
