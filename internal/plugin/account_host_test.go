package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

func nativeAccountHost(t *testing.T) (context.Context, string, string) {
	t.Helper()
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	file, err := filepath.Abs("testdata/nativeaccount/index.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, file); err != nil {
		t.Fatal(err)
	}
	first, err := Import(ctx, "nativefixture", map[string]any{"type": "oauth", "refresh": "one", "accountId": "same user", "accountScope": "scope-one", "accountEpoch": "epoch-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Import(ctx, "nativefixture", map[string]any{"type": "oauth", "refresh": "two", "accountId": "same user", "accountScope": "scope-two", "accountEpoch": "epoch-two"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different selections of the same identity were merged")
	}
	return ctx, first, second
}

func TestNativeAccountSelectionAndCAS(t *testing.T) {
	ctx, first, second := nativeAccountHost(t)
	ps, err := Providers(ctx)
	if err != nil || len(ps) != 1 || len(ps[0].Account) == 0 {
		t.Fatalf("account operations not advertised: %+v, %v", ps, err)
	}
	captured, err := AccountOperation(ctx, "nativefixture", first, AccountRequest{Operation: "context"})
	if err != nil || captured.Context.Scope != "scope-one" || captured.Context.Generation != "epoch-one" {
		t.Fatalf("capture = %+v, %v", captured, err)
	}
	req := AccountRequest{Operation: "selection.select", Scope: captured.Context.Scope, Generation: captured.Context.Generation,
		Params: map[string]any{"selection": map[string]any{"scope": "scope-two", "kind": "individual"}}}
	selected, err := AccountOperation(ctx, "nativefixture", first, req)
	if err != nil || selected.Context.Scope != "scope-two" || selected.Context.Generation != "epoch-two" {
		t.Fatalf("existing selection = %+v, %v", selected, err)
	}
	var data struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(selected.Data, &data); err != nil || data.Account != second {
		t.Fatalf("existing slot = %s, %v", selected.Data, err)
	}
	if a := Auths("nativefixture")[second]; a["refresh"] != "two" || a["accountEpoch"] != "epoch-two" {
		t.Fatalf("existing selection was replaced: %+v", a)
	}
	req.Params = map[string]any{"selection": map[string]any{"scope": "scope-three", "kind": "start"}}
	selected, err = AccountOperation(ctx, "nativefixture", first, req)
	if err != nil || selected.Context.Scope != "scope-three" || selected.Context.Generation == "" || selected.Context.Generation == "epoch-one" {
		t.Fatalf("new selection = %+v, %v", selected, err)
	}
	if strings.Contains(string(selected.Data), "selection-secret") {
		t.Fatal("selection returned private auth")
	}
	if a := Auths("nativefixture")[first]; a["refresh"] != "one" || a["accountEpoch"] != "epoch-one" {
		t.Fatalf("selection overwrote the source account: %+v", a)
	}
	cas, err := AccountOperation(ctx, "nativefixture", first, AccountRequest{Operation: "cas", Scope: "scope-one", Generation: "epoch-one"})
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct{ Saved, Conflict bool }
	if err := json.Unmarshal(cas.Data, &outcome); err != nil || outcome.Saved || !outcome.Conflict {
		t.Fatalf("stale CAS = %s, %v", cas.Data, err)
	}
	if got := Auths("nativefixture")[first]["secret"]; got != "newer" {
		t.Fatalf("stale CAS overwrote newer credentials: %v", got)
	}
}

func TestNativeAccountPreflightAndTypedErrors(t *testing.T) {
	ctx, first, _ := nativeAccountHost(t)
	mcp := AccountRequest{Operation: "mcp.headers", Params: map[string]any{"url": "https://attacker.invalid/mcp"}}
	_, err := AccountOperation(ctx, "nativefixture", "nativefixture#missing", mcp)
	var ae *AccountError
	if !errors.As(err, &ae) || ae.Kind != "untrusted_url" {
		t.Fatalf("untrusted URL was not rejected before account lookup: %v", err)
	}
	mcp.Params = map[string]any{"url": "https://trusted.invalid/mcp"}
	_, err = AccountOperation(ctx, "nativefixture", "nativefixture#missing", mcp)
	if !errors.As(err, &ae) || ae.Kind != "unavailable" {
		t.Fatalf("trusted URL and missing account: %v", err)
	}
	_, err = AccountOperation(ctx, "nativefixture", first, AccountRequest{Operation: "error", Scope: "scope-one", Generation: "epoch-one"})
	if !errors.As(err, &ae) || ae.Kind != "throttled" || ae.Code != 42901 || ae.HTTPStatus != 429 || !ae.Retryable || !ae.Throttled || !ae.Dispatched || ae.RequestID != "req-1" || ae.NextTakeAt != 1800000000000 || ae.NextDrawAt != 1800000001000 || ae.RetryAfter != 2.5 {
		t.Fatalf("typed error = %+v (%v)", ae, err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("private exception message leaked: %v", err)
	}
	if _, err := AccountOperation(ctx, "nativefixture", "", AccountRequest{Operation: "context"}); err == nil {
		t.Fatal("empty selector fell back to the first account")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = AccountOperation(cctx, "nativefixture", first, AccountRequest{Operation: "wait", Scope: "scope-one", Generation: "epoch-one"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled account operation = %v", err)
	}
}

func TestNativeAccountHostDispatchRoundTrip(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path := filepath.Join(t.TempDir(), "index.mjs")
	source := `export default async () => ({
  config: async (cfg) => {
    cfg.provider ??= {}
    cfg.provider.dispatchfixture = { name: "Dispatch fixture", npm: "@ai-sdk/openai-compatible",
      api: "https://fixture.invalid/v1", models: { "fixture-1": { name: "Fixture", limit: { context: 1000, output: 100 } } } }
  },
  auth: {
    provider: "dispatchfixture", methods: [{ type: "api", label: "key" }], accountOperations: ["context", "fail"],
    async account(_getAuth, _provider, { operation, params }) {
      if (operation === "context") return { context: { scope: "scope-one", generation: "epoch-one" }, data: {} }
      const error = Object.assign(new Error("private fixture details"), { kind: "network", code: 123, retryable: true })
      if (params.mode === "before") error.dispatched = false
      if (params.mode === "after") error.dispatched = true
      throw error
    },
  },
})`
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, path); err != nil {
		t.Fatal(err)
	}
	account, err := Import(ctx, "dispatchfixture", map[string]any{"type": "oauth", "refresh": "fixture", "accountId": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mode        string
		dispatched, known bool
	}{
		{"pre-dispatch", "before", false, true},
		{"post-dispatch", "after", true, true},
		{"unknown", "unknown", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AccountOperation(ctx, "dispatchfixture", account, AccountRequest{
				Operation: "fail", Scope: "scope-one", Generation: "epoch-one", Params: map[string]any{"mode": tc.mode},
			})
			var accountErr *AccountError
			if !errors.As(err, &accountErr) || accountErr.Kind != "network" || accountErr.Code != 123 || !accountErr.Retryable {
				t.Fatalf("host account error = %+v (%v)", accountErr, err)
			}
			check := func(stage string, ae *AccountError) {
				t.Helper()
				if ae == nil || ae.Dispatched != tc.dispatched || ae.DispatchedKnown != tc.known {
					t.Fatalf("%s dispatch = %+v, want value=%v known=%v", stage, ae, tc.dispatched, tc.known)
				}
			}
			check("host", accountErr)
		})
	}
}

func TestAccountErrorDispatchJSONConstructors(t *testing.T) {
	for _, tc := range []struct {
		name              string
		input             AccountError
		dispatched, known bool
	}{
		{"unknown", AccountError{Kind: "unknown"}, false, false},
		{"known false", AccountError{Kind: "invalid", DispatchedKnown: true}, false, true},
		{"known true", AccountError{Kind: "network", Dispatched: true, DispatchedKnown: true}, true, true},
		{"legacy true literal", AccountError{Kind: "network", Dispatched: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			if _, present := raw["dispatched"]; present != tc.known {
				t.Fatalf("presence = %v, want %v: %s", present, tc.known, data)
			}
			var result AccountError
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Kind != tc.input.Kind || result.Dispatched != tc.dispatched || result.DispatchedKnown != tc.known {
				t.Fatalf("roundtrip = %+v, want value=%v known=%v", result, tc.dispatched, tc.known)
			}
		})
	}
	// Reusing a destination cannot turn an absent or null flag into the
	// previous operation's known value.
	for _, data := range []string{`{"kind":"unknown"}`, `{"kind":"unknown","dispatched":null}`} {
		result := AccountError{Dispatched: true, DispatchedKnown: true}
		if err := json.Unmarshal([]byte(data), &result); err != nil || result.Dispatched || result.DispatchedKnown {
			t.Fatalf("decode %s = %+v (%v)", data, result, err)
		}
	}
}

func TestNativeAccountLoginEpochChangesOnRelogin(t *testing.T) {
	ctx, _, _ := nativeAccountHost(t)
	saved, err := APIKey(ctx, "nativefixture", 0, nil, "first-key", NewAccount)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := Auths("nativefixture")[saved.Account]["accountEpoch"].(string)
	if before == "" {
		t.Fatal("the first login had no durable epoch")
	}
	again, err := APIKey(ctx, "nativefixture", 0, nil, "replacement-key", saved.Account)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := Auths("nativefixture")[again.Account]["accountEpoch"].(string)
	if after == "" || before == after {
		t.Fatalf("a real re-login kept the old epoch: %q", after)
	}
}

func TestNativeUsageObservations(t *testing.T) {
	ctx, first, _ := nativeAccountHost(t)
	u, err := AccountUsage(ctx, "nativefixture", first)
	if err != nil || u.Resets == nil || u.Resets.Source != "legacy-list" || len(u.Observations) != 2 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	start := u.Observations[0]
	if start.Kind != "start-balance" || start.Used == nil || *start.Used != 0 || start.Remaining == nil || *start.Remaining != 20 || start.Identity == nil || start.Identity.UserPlanID != "plan-1" || start.Identity.ExpiresAt != "2026-12-01T00:00:00.000Z" {
		t.Fatalf("start observation = %+v", start)
	}
	if u.Observations[1].Used != nil {
		t.Fatalf("invalid usage became a zero: %+v", u.Observations[1])
	}
	b, _ := json.Marshal(u.Observations)
	if strings.Contains(string(b), "do-not-copy") {
		t.Fatalf("usage leaked an undeclared field: %s", b)
	}
}

func TestNativeFetchPrivateBinding(t *testing.T) {
	ctx, first, _ := nativeAccountHost(t)
	req := FetchRequest{Provider: "nativefixture", Account: first, Model: "GLM-5.3",
		RequestedModel: "GLM-5.3-OffPeak", RequestedSession: "task-1", Session: "different-chat-session",
		Execution: json.RawMessage(`{"lane":"offpeak","taskId":"task-1","scope":"scope-one"}`),
		URL:       "https://fixture.invalid/v1/messages", Method: "POST"}
	resp, err := Fetch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the admitted binding was not passed to the loader: %d", resp.StatusCode)
	}
	req.RequestedSession = "other-task"
	resp, err = Fetch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("a different session received the admitted lane: %d", resp.StatusCode)
	}
}

func TestNativeResetCallsThroughHost(t *testing.T) {
	sandbox(t)
	t.Setenv("NATIVE_FIXTURE_PROVIDER", "nativefixture")
	var mu sync.Mutex
	var calls []string
	var use map[string]any
	testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			_, _ = w.Write([]byte(`{"available_five_hour_resets":[{"expire_at":1900000000000}],"available_week_resets":[],"latest_five_hour_reset_history":null,"latest_week_reset_history":null,"has_unread_history":false}`))
		case "/v1/opportunity":
			_, _ = w.Write([]byte(`{"granted":true}`))
		case "/v1/use":
			_ = json.NewDecoder(r.Body).Decode(&use)
			_, _ = w.Write([]byte(`{"used":true}`))
		case "/v1/history/read":
			_, _ = w.Write([]byte(`null`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	file, err := filepath.Abs("testdata/nativeaccount/index.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, file); err != nil {
		t.Fatal(err)
	}
	key, err := Import(ctx, "nativefixture", map[string]any{
		"type": "oauth", "refresh": "fixture", "accountScope": "scope-one", "accountEpoch": "epoch-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(operation string, params any) AccountResult {
		t.Helper()
		result, err := AccountOperation(ctx, "nativefixture", key, AccountRequest{
			Operation: operation, Params: params, Scope: "scope-one", Generation: "epoch-one",
		})
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		return result
	}
	status := ask("reset.status", nil)
	var report struct {
		Five []struct {
			ExpireAt int64 `json:"expire_at"`
		} `json:"available_five_hour_resets"`
	}
	if err := json.Unmarshal(status.Data, &report); err != nil || len(report.Five) != 1 || report.Five[0].ExpireAt != 1900000000000 {
		t.Fatalf("status = %s, %v", status.Data, err)
	}
	ask("reset.opportunity", map[string]any{"idempotency_key": "opportunity-key"})
	result := ask("reset.use", map[string]any{"idempotency_key": "use-key", "reset_type": "FIVE_HOUR"})
	var used struct {
		Used bool `json:"used"`
	}
	if err := json.Unmarshal(result.Data, &used); err != nil || !used.Used {
		t.Fatalf("use = %s, %v", result.Data, err)
	}
	ask("reset.history.read", nil)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(calls, ","); got != "GET /v1/status,POST /v1/opportunity,POST /v1/use,POST /v1/history/read" {
		t.Fatalf("reset requests = %s", got)
	}
	if use["idempotency_key"] != "use-key" || use["reset_type"] != "FIVE_HOUR" {
		t.Fatalf("reset use parameters = %+v", use)
	}
}
