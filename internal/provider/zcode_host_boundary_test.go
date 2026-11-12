package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	fixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
)

func TestZCodePartialUsageAcrossEmbeddedHostAndLoginCache(t *testing.T) {
	claudeHome(t)
	fixture.Start(t)
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": v}) }
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			reply(map[string]any{"accessToken": "project-token", "tokenType": "Bearer", "expiresIn": 3600, "expiresAt": time.Now().Unix() + 3600})
		case strings.HasSuffix(r.URL.Path, "/quota/limit"):
			if mode.Load() == 0 || mode.Load() == 4 {
				reply(map[string]any{"limits": []any{map[string]any{"type": "TOKENS_LIMIT", "unit": 3, "number": 5, "percentage": 25}}})
			} else {
				http.Error(w, "paid unavailable", http.StatusServiceUnavailable)
			}
		case r.URL.Path == "/api/v1/zcode-plan/billing/balance" && mode.Load() == 1:
			now := time.Now().Unix()
			reply(map[string]any{"server_time": now, "plans": []any{map[string]any{"plan_id": "start", "user_plan_id": "u", "name": "Start", "status": "active", "ends_at": now + 3600}},
				"balances": []any{map[string]any{"plan_id": "start", "user_plan_id": "u", "show_name": "GLM-5.3", "capabilities": []string{"model:GLM-5.3"}, "total_units": 100, "remaining_units": 60}}})
		case r.URL.Path == "/api/v1/mcp/usage" && mode.Load() < 3:
			reply(map[string]any{"server_time": time.Now().Unix(), "total_usage": map[string]any{"used": 4, "limit": 20, "remaining": 16}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("ZCODE_FIXTURE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := fixture.Install(t, ctx)
	logins, ok := usageLogins("plugin:zcode")
	if !ok || len(logins) != 1 {
		t.Fatalf("fixture logins: %+v", logins)
	}
	l := logins[0]
	first := LoginUsageShown(Asked(ctx), l.Agent)[l.User]
	if first.Error != "" || first.ReadAt == nil || len(first.Windows) != 1 || first.Windows[0].Used != 25 {
		t.Fatalf("initial paid read: %+v", first)
	}
	age := func() {
		key := loginReadingKey(l)
		loginUsageCache.Lock()
		e := loginUsageCache.m[key]
		e.at = time.Now().Add(-2 * time.Minute)
		loginUsageCache.m[key] = e
		loginUsageCache.Unlock()
	}
	mode.Store(1)
	age()
	routing := LoginUsage(Asked(ctx), l.Agent)[l.User]
	reading := loginReading(Asked(ctx), l).read
	key := quotaHistoryKey(reading)
	history := readQuotaHist()[key]
	if len(reading.Windows) != 1 || len(history[reading.Windows[0].LineName()]) != 1 || len(history[first.Windows[0].LineName()]) != 1 {
		t.Fatalf("LoginUsage-only history missed the fresh subset: %+v; read=%+v", history, reading)
	}
	shown := LoginUsageShown(Asked(ctx), l.Agent)[l.User]
	if routing.Error != "" || len(routing.Windows) != 1 || routing.Windows[0].Name != first.Windows[0].Name {
		t.Fatalf("routing fallback changed: %+v", routing)
	}
	if !shown.Partial || shown.Error != "Coding Plan quota unavailable" || shown.ReadAt == nil || shown.AsOf != nil ||
		len(shown.Windows) != 1 || shown.Windows[0].Used != 40 || len(shown.Observations) != 2 {
		t.Fatalf("shown partial: %+v", shown)
	}
	wire, err := json.Marshal(quotaReport([]SubscriptionQuota{shown}, nil, nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var report []Quota
	if err := json.Unmarshal(wire, &report); err != nil || len(report) != 1 || !report[0].Partial ||
		report[0].Error != shown.Error || len(report[0].Windows) != 1 || len(report[0].Observations) != 2 {
		t.Fatalf("public quota JSON lost partial data: %s, %v", wire, err)
	}
	again := LoginUsageShown(Asked(ctx), l.Agent)[l.User]
	if again.ReadAt == nil || !again.ReadAt.Equal(*shown.ReadAt) {
		t.Fatalf("cached partial was restamped: first=%+v again=%+v", shown, again)
	}
	noteQuotaHistory([]SubscriptionQuota{again}, shown.ReadAt.Add(time.Hour))
	if n := len(readQuotaHist()[key][shown.Windows[0].LineName()]); n != 1 {
		t.Fatalf("cached partial made %d history samples", n)
	}
	mode.Store(2)
	age()
	mcp := LoginUsageShown(Asked(ctx), l.Agent)[l.User]
	if !mcp.Partial || mcp.Error == "" || mcp.ReadAt == nil || len(mcp.Windows) != 0 || len(mcp.Observations) != 1 ||
		mcp.Observations[0].Kind != "server-mcp" || mcp.Observations[0].Remaining == nil || *mcp.Observations[0].Remaining != 16 {
		t.Fatalf("MCP-only host result: %+v", mcp)
	}
	repeated := LoginUsageShown(Asked(ctx), l.Agent)[l.User]
	if repeated.ReadAt == nil || !repeated.ReadAt.Equal(*mcp.ReadAt) {
		t.Fatalf("MCP-only partial was restamped: %+v", repeated)
	}
	mode.Store(3)
	full, err := plugin.AccountUsage(ctx, "zcode", account)
	if err != nil || full.Error == "" || full.Partial || len(full.Windows) != 0 || len(full.Observations) != 0 {
		t.Fatalf("full failure: %+v %v", full, err)
	}
	mode.Store(4)
	good, err := plugin.AccountUsage(ctx, "zcode", account)
	if err != nil || good.Error != "" || good.Partial || len(good.Windows) != 1 {
		t.Fatalf("optional source failure lost paid quota: %+v %v", good, err)
	}
	mode.Store(1)
	expiredAuth := fixture.AuthFor("expired-user", "", "fixture-zcode")
	var state map[string]any
	if err := json.Unmarshal([]byte(expiredAuth["refresh"].(string)), &state); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
	state["oauthToken"] = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	raw, _ := json.Marshal(state)
	expiredAuth["refresh"] = string(raw)
	expiredAccount, err := plugin.Import(ctx, "zcode", expiredAuth)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := plugin.AccountUsage(ctx, "zcode", expiredAccount)
	if err != nil || expired.SignIn != "expired" || expired.Error == "" || !expired.Partial || len(expired.Windows) == 0 {
		t.Fatalf("expired mandatory sign-in with independent Start: %+v %v", expired, err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = usageLogins("plugin:zcode")
	expiredUser := PluginAccountUser("zcode", expiredAccount)
	if expiredUser == "" {
		t.Fatal("expired account has no visible login")
	}
	expiredShown := LoginUsageShown(Asked(ctx), "plugin:zcode")[expiredUser]
	if !expiredShown.Partial || expiredShown.Error == "" || len(expiredShown.Windows) == 0 {
		t.Fatalf("expired account view lost independent Start: %+v", expiredShown)
	}
	var lapsed bool
	for _, saved := range readLogins() {
		if saved.Agent == "plugin:zcode" && saved.Home == expiredAccount {
			lapsed = saved.Lapsed != ""
		}
	}
	if !lapsed {
		t.Fatal("mandatory expired sign-in was not marked lapsed")
	}
}

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
