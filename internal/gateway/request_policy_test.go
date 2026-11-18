package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func TestSharedPolicyGatewayDisable(t *testing.T) {
	fresh(t)
	s := settings.Load()
	if err := json.Unmarshal([]byte(`{"requestPolicy":{"defaults":{"maxRetries":0}}}`), &s); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if wait, ok := passing(503, nil, []byte(`{"error":{"message":"temporarily unavailable"}}`), 0); ok {
		t.Fatalf("disabled shared retry policy still admits a retry after %v", wait)
	}
}

func TestSharedPolicyShortWaitBeyondDeadlineUsesReadySibling(t *testing.T) {
	shortWait, maxDelay := 45000, 60000
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = requestpolicy.WithConfig(ctx, &requestpolicy.Config{Defaults: requestpolicy.Options{ShortWaitMS: &shortWait, MaxDelayMS: &maxDelay}})
	selected := candidateOf(provider.Provider{ID: "fixture"}, "free")
	refusal := &requestpolicy.Refusal{Classification: requestpolicy.Classification{Class: requestpolicy.Quota, Scope: "model", Rule: "fixture.free"}, Status: 429, Step: "inference", NoEffect: true, NextAllowed: time.Now().Add(40 * time.Second)}
	for _, hasNext := range []bool{true, false} {
		decision := resolveFinalPolicy(ctx, selected, refusal, hasNext, func(context.Context, time.Duration) error {
			t.Fatal("wait exceeds caller deadline")
			return nil
		})
		want := requestpolicy.ActionTerminal
		if hasNext {
			want = requestpolicy.ActionNext
		}
		if decision.Action != want || !decision.NextAllowed.Equal(refusal.NextAllowed) {
			t.Fatalf("hasNext=%v decision=%+v", hasNext, decision)
		}
	}
	if total, _ := requestpolicy.FromContext(ctx).Counts("fixture"); total != 0 {
		t.Fatal("unusable wait consumed the sibling's dispatch permit", total)
	}
}

func TestSharedPolicyEveryApplicableWindowConstrainsTotalBudget(t *testing.T) {
	shortWait, maxDelay, available, exhausted := 45000, 60000, 2, 0
	ctx := requestpolicy.WithConfig(context.Background(), &requestpolicy.Config{Defaults: requestpolicy.Options{
		ShortWaitMS: &shortWait, MaxDelayMS: &maxDelay,
		Windows: map[string]requestpolicy.Options{"free": {MaxTotalRetries: &available}, "daily": {MaxTotalRetries: &exhausted}},
	}})
	selected := candidateOf(provider.Provider{ID: "fixture"}, "free")
	refusal := &requestpolicy.Refusal{Classification: requestpolicy.Classification{Class: requestpolicy.Quota, Scope: "model", Rule: "fixture.free"}, Status: 429, Step: "inference", NoEffect: true, NextAllowed: time.Now().Add(40 * time.Second), Windows: []requestpolicy.Window{{ID: "free"}, {ID: "daily"}}}
	waited := false
	decision := resolveFinalPolicy(ctx, selected, refusal, false, func(context.Context, time.Duration) error {
		waited = true
		return nil
	})
	if waited || decision.Action != requestpolicy.ActionTerminal {
		t.Fatalf("exhausted second window ignored: waited=%v decision=%+v", waited, decision)
	}
	if total, _ := requestpolicy.FromContext(ctx).Counts("fixture"); total != 0 {
		t.Fatal("denied window consumed a permit", total)
	}
}

func TestSharedPolicyGroupChargesEveryRepeatedProvider(t *testing.T) {
	for _, test := range []struct {
		name, policy  string
		first, second int
	}{
		{"disabled first", `{"defaults":{"maxTotalRetries":8},"providers":{"first":{"maxRetries":0},"second":{"maxRetries":1}}}`, 1, 2},
		{"one root permit", `{"defaults":{"maxTotalRetries":1}}`, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			scaled(t, time.Second)
			current := settings.Load()
			if err := json.Unmarshal([]byte(`{"requestPolicy":`+test.policy+`}`), &current); err != nil {
				t.Fatal(err)
			}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			first := &scripted{replies: []reply{{503, "", `{"error":{"message":"temporarily unavailable"}}`}}}
			second := &scripted{replies: []reply{{503, "", `{"error":{"message":"temporarily unavailable"}}`}}}
			scriptedOn(t, "first", provider.Chat, first)
			scriptedOn(t, "second", provider.Chat, second)
			if err := provider.SaveGroup(provider.Group{ID: "policy-group", Name: "Policy group", Members: []string{"first/m", "second/m"}, Routing: "order"}); err != nil {
				t.Fatal(err)
			}
			status, _ := postAs(t, New(), "", `{"model":"policy-group","messages":[{"role":"user","content":"hi"}]}`)
			if status != 503 || first.n != test.first || second.n != test.second {
				t.Fatalf("status=%d sends=(%d,%d), want (%d,%d)", status, first.n, second.n, test.first, test.second)
			}
		})
	}
}

func TestSharedPolicyMalformedEnvelopeNeverAuthorizesRetry(t *testing.T) {
	fresh(t)
	for _, body := range []string{` {"error":{"message":"busy"}`, `{"error":{"message":"busy"}} {}`, `{"error":{"code":{}}}`, `{"error":null}`, `{"error":{"code":null}}`, `{"request_id":"1303"}`, `{"error":{"code":"1303","code":"1308"}}`} {
		if facts := responseFacts(503, nil, []byte(body)); facts.Complete {
			t.Errorf("malformed or unknown envelope trusted: %q", body)
		}
		if _, retry := passingFor(context.Background(), "fixture", 503, nil, []byte(body), 0); retry {
			t.Errorf("malformed or unknown envelope permits repeat: %q", body)
		}
	}
	for _, body := range []string{`{"error":{"code":1303}}`, `{"error":{"type":"rate_limit_error"}}`, `{"error":{"message":"busy"}}`, `{"error":{"code":null,"message":"busy"}}`} {
		if facts := responseFacts(429, nil, []byte(body)); !facts.Complete {
			t.Errorf("valid machine envelope lost: %q", body)
		}
	}
}

func TestSharedPolicyPreservesFractionalAndResetHints(t *testing.T) {
	fresh(t)
	base := retryPause
	retryPause = time.Millisecond
	t.Cleanup(func() { retryPause = base })
	for _, headers := range []http.Header{{"Retry-After": {"0.5"}}, {"X-Ratelimit-Reset-Requests": {"500ms"}}} {
		wait, retry := passing(503, headers, []byte(`{"error":{"message":"busy"}}`), 0)
		if !retry || wait != 500*time.Millisecond {
			t.Fatalf("headers=%v wait=%v retry=%v", headers, wait, retry)
		}
	}
}

func TestSharedPolicyPatienceAdmissionPreservesLongDefaultWait(t *testing.T) {
	fresh(t)
	ctx := requestpolicy.WithConfig(context.Background(), nil)
	if !admitRequestRetry(ctx, "fixture", 30*time.Second) {
		t.Fatal("default policy removed existing 30s patience pause")
	}
}

func TestSharedPolicyActualRouteLimitIncludesPatience(t *testing.T) {
	fresh(t)
	current := settings.Load()
	if err := json.Unmarshal([]byte(`{"requestPolicy":{"defaults":{"maxRetries":1,"baseDelayMs":0}}}`), &current); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	upstream := &scripted{replies: []reply{{503, "", `{"error":{"message":"temporarily unavailable"}}`}}}
	scriptedOn(t, "limited", provider.Chat, upstream)
	status, _ := postAs(t, New(), "", `{"model":"limited/m","messages":[{"role":"user","content":"hi"}]}`)
	if status != 503 || upstream.n != 2 {
		t.Fatalf("status=%d sends=%d; policy allowed only one retry including patience", status, upstream.n)
	}
}

func TestSharedPolicyActualProviderOverride(t *testing.T) {
	fresh(t)
	current := settings.Load()
	if err := json.Unmarshal([]byte(`{"requestPolicy":{"defaults":{"maxRetries":0},"providers":{"allowed":{"maxRetries":1,"baseDelayMs":0}}}}`), &current); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"allowed", "neighbor"} {
		upstream := &scripted{replies: []reply{{503, "", `{"error":{"message":"temporarily unavailable"}}`}, {200, "", chatOK}}}
		scriptedOn(t, id, provider.Chat, upstream)
		status, _ := postAs(t, New(), "", `{"model":"`+id+`/m","messages":[{"role":"user","content":"hi"}]}`)
		if id == "allowed" && (status != 200 || upstream.n != 2) || id == "neighbor" && (status != 503 || upstream.n != 1) {
			t.Fatalf("%s status=%d sends=%d", id, status, upstream.n)
		}
	}
}

func TestSharedPolicyLegacyClassifierRemainsTheRoutingOwner(t *testing.T) {
	fresh(t)
	current := settings.Load()
	if err := json.Unmarshal([]byte(`{"requestPolicy":{"defaults":{"rules":[{"id":"transport-only","status":[503],"class":"quota","scope":"account"}]}}}`), &current); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	// Transport rules do not change just one part of legacy routing while its
	// cooldown/account-fallback owners continue classifying the original body.
	if _, ok := passing(503, nil, []byte(`{"error":{"message":"temporarily unavailable"}}`), 0); !ok {
		t.Fatal("transport rule partially overrode legacy routing")
	}
}

func TestSharedPolicyRetryAfterIsMinimum(t *testing.T) {
	fresh(t)
	old := retryPause
	retryPause = 2 * time.Second
	t.Cleanup(func() { retryPause = old })
	wait, ok := passing(503, http.Header{"Retry-After": {"1"}}, []byte(`{"error":{"message":"temporarily unavailable"}}`), 0)
	if !ok || wait != 2*time.Second {
		t.Fatalf("retry after %v (%v), want base delay 2s despite shorter server minimum", wait, ok)
	}
}

func TestFinalRefusalFallbackChecksDestinationBudget(t *testing.T) {
	fresh(t)
	zero := 0
	for _, total := range []int{0, 1} {
		ctx := requestpolicy.WithConfig(t.Context(), &requestpolicy.Config{
			Defaults:  requestpolicy.Options{MaxTotalRetries: &total},
			Providers: map[string]requestpolicy.Options{"spent": {MaxTotalRetries: &zero}},
		})
		candidates := []candidate{candidateOf(provider.Provider{ID: "spent"}, "first"), candidateOf(provider.Provider{ID: "spent"}, "second"), candidateOf(provider.Provider{ID: "ready"}, "third")}
		selected := candidates[0]
		ctx = requestpolicy.WithSelection(ctx, selected.restID(), selected.model)
		policy, err := requestPolicy(ctx, selected.p.ID)
		if err != nil {
			t.Fatal(err)
		}
		budget := requestpolicy.FromContext(ctx)
		if err := budget.Dispatch(ctx, policy); err != nil {
			t.Fatal(err)
		}
		next := nextPolicyCandidate(ctx, candidates, 1)
		refusal := &requestpolicy.Refusal{Classification: requestpolicy.Classification{Class: requestpolicy.Auth, Scope: "account", Rule: "fixture.auth"}, Status: 401, NoEffect: true}
		decision := resolveFinalPolicy(ctx, selected, refusal, next >= 0, nil)
		if total == 0 && (next >= 0 || decision.Action != requestpolicy.ActionTerminal) || total == 1 && (next != 2 || decision.Action != requestpolicy.ActionNext) {
			t.Fatalf("root permits=%d next=%d decision=%+v", total, next, decision)
		}
		if used, _ := budget.Counts("ready"); used != 0 {
			t.Fatalf("checking fallback consumed %d retries", used)
		}
	}
}
