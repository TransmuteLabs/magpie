package requestpolicy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ptr(n int) *int { return &n }
func facts(status int, code, message string) Facts {
	return Facts{Status: status, Code: code, Message: message, Operation: "chat", Phase: "response", Complete: true}
}

func TestClassificationRulesAreConjunctiveAndProviderScoped(t *testing.T) {
	c := &Config{Defaults: Options{Rules: []Rule{{ID: "global", Status: []int{429}, Contains: "rate", Class: Rate, Scope: "account"}}}, Providers: map[string]Options{
		"vendor": {Rules: []Rule{
			{ID: "budget", Status: []int{429}, Code: "1308", Contains: "limit", Class: Quota, Scope: "account"},
			{ID: "pace", Status: []int{429}, Code: "1303", Class: Rate, Scope: "account"},
		}},
	}}
	p, err := Resolve(c, "vendor", Defaults())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		f     Facts
		class Class
		rule  string
	}{
		{facts(429, "1308", "rate limit reached"), Quota, "budget"},
		{facts(429, "1303", "limit reached"), Rate, "pace"},
		{facts(403, "1308", "limit reached"), Unknown, "unknown"},
		{facts(429, "", "request id 1308"), Unknown, "unknown"},
		{facts(429, "1308", "permission denied"), Unknown, "unknown"},
	} {
		got := p.Classify(tc.f, Classification{})
		if got.Class != tc.class || got.Rule != tc.rule {
			t.Errorf("%+v => %+v", tc.f, got)
		}
	}
	other, _ := Resolve(c, "neighbor", Defaults())
	if got := other.Classify(facts(429, "1308", "rate limit reached"), Classification{}); got.Class != Rate || got.Rule != "global" {
		t.Fatal(got)
	}
}

func TestRetrySafetyCannotBeOverridden(t *testing.T) {
	p, _ := Resolve(&Config{Defaults: Options{MaxRetries: ptr(64), Rules: []Rule{{ID: "operator", Status: []int{500}, Class: Outage, Scope: "request"}}}}, "p", Defaults())
	for _, s := range []Safety{{}, {NoEffect: true, Accepted: true}, {NoEffect: true, Uncertain: true}, {NoEffect: true, OutputStarted: true}} {
		if d := p.Decide(facts(500, "", ""), Classification{}, s, 0, time.Now(), 0); d.Retry {
			t.Fatal(d)
		}
	}
	if d := Defaults().Decide(facts(429, "", ""), Classification{}, Safety{NoEffect: true}, 0, time.Now(), 0); d.Retry {
		t.Fatal("status alone supplied retry proof")
	}
}

func TestRetryDelayAndBounds(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	p := Defaults()
	p.JitterPercent = 50
	f := facts(429, "", "rate")
	c := Classification{Rate, "account", "vendor.rate"}
	if d := p.Decide(f, c, Safety{NoEffect: true}, 1, now, 0.5); d.Delay != 2500*time.Millisecond || !d.Retry {
		t.Fatal(d)
	}
	f.Headers = map[string]string{"Retry-After": "6"}
	if d := p.Decide(f, c, Safety{NoEffect: true}, 0, now, 0); d.Delay != 6*time.Second || !d.Retry {
		t.Fatal(d)
	}
	f.Headers["Retry-After"] = "9"
	if d := p.Decide(f, c, Safety{NoEffect: true}, 0, now, 0); d.Retry {
		t.Fatal("shortened Retry-After", d)
	}
	f.Headers["Retry-After"] = now.Add(7 * time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT")
	if d := p.Decide(f, c, Safety{NoEffect: true}, 0, now, 0); d.Delay != 7*time.Second {
		t.Fatal(d)
	}
	if d := p.Decide(f, c, Safety{NoEffect: true}, 3, now, 0); d.Retry {
		t.Fatal(d)
	}
}

func TestJitterCapsScheduleButNeverShortensRetryAfter(t *testing.T) {
	policy := Defaults()
	policy.MaxRetries = ptr(8)
	policy.JitterPercent = 20
	failure := facts(503, "busy", "busy")
	classification := Classification{Class: Outage, Scope: "request", Rule: "outage"}
	decision := policy.Decide(failure, classification, Safety{NoEffect: true}, 3, time.Now(), 0.5)
	if !decision.Retry || decision.Delay != 8*time.Second {
		t.Fatalf("jitter removed a permitted retry: %+v", decision)
	}
	failure.Headers = map[string]string{"Retry-After": "9"}
	if got := policy.Decide(failure, classification, Safety{NoEffect: true}, 3, time.Now(), 0.5); got.Retry {
		t.Fatal("server minimum was shortened", got)
	}
}

func TestVendorStableRulesPrecedeProseAndAllPredicatesApply(t *testing.T) {
	policy := Defaults()
	rules := []Rule{
		{ID: "generic-pace", Contains: "limit", Class: Rate, Scope: "account"},
		{ID: "quota-code", Status: []int{429}, Code: "budget", Class: Quota, Scope: "account"},
		{ID: "bad-request", Status: []int{401}, Code: "invalid_request", Class: Request, Scope: "request"},
		{ID: "envelope-quota", Status: []int{200}, Type: "budget", Contains: "exhausted", Headers: map[string]string{"X-Result": "refused"}, Operation: "chat", Phase: "response", Class: Quota, Scope: "account"},
	}
	if got := policy.ClassifyVendor(facts(429, "budget", "limit reached"), rules); got.Class != Quota {
		t.Fatal(got)
	}
	if got := policy.ClassifyVendor(facts(401, "invalid_request", "bad input"), rules); got.Class != Request {
		t.Fatal(got)
	}
	envelope := facts(200, "", "budget exhausted")
	envelope.Type = "budget"
	envelope.Headers = map[string]string{"x-result": "refused"}
	if got := policy.ClassifyVendor(envelope, rules); got.Class != Quota {
		t.Fatal(got)
	}
	for _, mutate := range []func(*Facts){func(f *Facts) { f.Headers = nil }, func(f *Facts) { f.Phase = "connect" }, func(f *Facts) { f.Operation = "media" }, func(f *Facts) { f.Type = "other" }, func(f *Facts) { f.Complete = false }} {
		neighbor := envelope
		mutate(&neighbor)
		if got := policy.ClassifyVendor(neighbor, rules); got.Class != Unknown {
			t.Fatal("AND predicate ignored", got)
		}
	}
	policy.Rules = []Rule{{ID: "operator-request", Status: []int{429}, Class: Request, Scope: "request"}}
	if got := policy.ClassifyVendor(facts(429, "budget", "limit"), rules); got.Rule != "operator-request" {
		t.Fatal(got)
	}
}

func TestSharedBudgetCancellationAndConcurrentAdmission(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	b := newBudget(func() time.Time { return now }, func() float64 { return 0 })
	p := Defaults()
	p.MaxTotalRetries = 3
	p.MaxElapsed = 10 * time.Second
	d := Decision{Retry: true, Delay: time.Second}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Admit(context.Background(), p, d) == nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 3 {
		t.Fatal(admitted.Load())
	}
	b = newBudget(func() time.Time { return now }, func() float64 { return 0 })
	now = now.Add(9 * time.Second)
	if !errors.Is(b.Admit(context.Background(), p, d), ErrDenied) {
		t.Fatal("delay escaped elapsed budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGlobalAndOverrideShareOneBudget(t *testing.T) {
	c := &Config{Defaults: Options{MaxTotalRetries: ptr(2)}, Providers: map[string]Options{"one": {MaxRetries: ptr(0)}, "two": {MaxRetries: ptr(4)}}}
	ctx := WithConfig(context.Background(), c)
	b := FromContext(ctx)
	if FromContext(WithConfig(ctx, nil)) != b {
		t.Fatal("nested request reset budget")
	}
	*c.Defaults.MaxTotalRetries = 99
	one, _ := b.Policy("one", Defaults())
	two, _ := b.Policy("two", Defaults())
	if one.MaxTotalRetries != 2 || *one.MaxRetries != 0 || *two.MaxRetries != 4 {
		t.Fatal(one, two)
	}
	for range 2 {
		if err := b.Admit(ctx, two, Decision{Retry: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Admit(ctx, one, Decision{Retry: true}); !errors.Is(err, ErrDenied) {
		t.Fatal("outer budget reset", err)
	}
}

func TestTypedRetryHintsUseHostClockAndStrongestMinimum(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	policy := Defaults()
	classification := Classification{Class: Rate, Scope: "account", Rule: "vendor.pace"}
	duration := int64(3000)
	failure := facts(429, "pace", "slow down")
	failure.Headers = map[string]string{"Retry-After": "2", "X-Ratelimit-Reset-Requests": "4s"}
	failure.RetryAfterMS = &duration
	failure.ResetAt = now.Add(5 * time.Second).Format(time.RFC3339)
	decision := policy.Decide(failure, classification, Safety{NoEffect: true}, 0, now, 0)
	if !decision.Retry || decision.Delay != 5*time.Second || !decision.NextAllowed.Equal(now.Add(5*time.Second)) {
		t.Fatal(decision)
	}
	policy.MaxDelay = 4 * time.Second
	decision = policy.Decide(failure, classification, Safety{NoEffect: true}, 0, now, 0)
	if decision.Retry || decision.Reason != "delay-limit" || !decision.NextAllowed.Equal(now.Add(5*time.Second)) {
		t.Fatal(decision)
	}
	policy.MaxDelay = 8 * time.Second
	policy.MaxElapsed = 5 * time.Second
	budget := newBudget(func() time.Time { return now }, func() float64 { return 0 })
	decision = budget.Decide(policy, failure, classification, Safety{NoEffect: true}, 0)
	if !errors.Is(budget.Admit(context.Background(), policy, decision), ErrDenied) {
		t.Fatal("hint escaped elapsed budget")
	}
	for _, mutate := range []func(*Facts){func(f *Facts) { bad := int64(-1); f.RetryAfterMS = &bad }, func(f *Facts) { f.ResetAt = "tomorrow" }, func(f *Facts) { f.Complete = false }} {
		invalid := failure
		mutate(&invalid)
		if got := policy.Decide(invalid, classification, Safety{NoEffect: true}, 0, now, 0); got.Retry || got.Class != Unknown {
			t.Fatal("invalid facts trusted", got)
		}
	}
}

func TestProviderSwitchCannotRaiseLogicalRequestBudget(t *testing.T) {
	ctx := WithConfig(context.Background(), &Config{Defaults: Options{MaxTotalRetries: ptr(1)}, Providers: map[string]Options{"second": {MaxTotalRetries: ptr(8)}}})
	budget := FromContext(ctx)
	first, _ := budget.Policy("first", Defaults())
	second, _ := budget.Policy("second", Defaults())
	if err := budget.Admit(ctx, first, Decision{Retry: true}); err != nil {
		t.Fatal(err)
	}
	if err := budget.Admit(ctx, second, Decision{Retry: true}); !errors.Is(err, ErrDenied) {
		t.Fatal("provider override replenished root", err)
	}
}

func TestDispositionBoundsShortWaitAndNeverSleepsOnHours(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	policy := Defaults()
	policy.ShortWait = time.Minute
	policy.MaxDelay = 3 * time.Hour
	classification := Classification{Class: Quota, Scope: "model", Rule: "window"}
	for _, test := range []struct {
		name   string
		delay  time.Duration
		action string
	}{
		{"short", 40 * time.Second, ActionWait}, {"minutes", 30 * time.Minute, ActionNext}, {"hours", time.Hour, ActionNext},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := facts(429, "quota", "window exhausted")
			failure.ResetAt = now.Add(test.delay).Format(time.RFC3339)
			decision := policy.Disposition(failure, classification, Safety{NoEffect: true}, 0, now, 0)
			if decision.Action != test.action || !decision.NextAllowed.Equal(now.Add(test.delay)) {
				t.Fatal(decision)
			}
		})
	}
	for _, scope := range []string{"request", "unknown"} {
		decision := policy.Disposition(facts(429, "quota", "unknown reset"), Classification{Class: Quota, Scope: scope, Rule: "bounded"}, Safety{NoEffect: true}, 0, now, 0)
		if decision.Action != ActionTerminal {
			t.Fatal("scope cannot enumerate siblings", decision)
		}
	}
	policy.MaxRetries = ptr(0)
	failure := facts(429, "quota", "known")
	failure.ResetAt = now.Add(40 * time.Second).Format(time.RFC3339)
	if decision := policy.Disposition(failure, classification, Safety{NoEffect: true}, 0, now, 0); decision.Retry {
		t.Fatal("disabled same-account retry", decision)
	}
}

func TestRootDispatchReservationAndSiblingCounters(t *testing.T) {
	total := 2
	ctx := WithConfig(context.Background(), &Config{Defaults: Options{MaxTotalRetries: &total}})
	budget := FromContext(ctx)
	policy, _ := budget.Policy("vendor", Defaults())
	alice := WithSelection(ctx, "vendor/alice/free")
	bob := WithSelection(ctx, "vendor/bob/free")
	if err := budget.Dispatch(alice, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Reserve(alice, policy, Decision{Retry: true}); err != nil {
		t.Fatal(err)
	}
	if err := budget.Dispatch(alice, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Dispatch(bob, policy); err != nil {
		t.Fatal(err)
	}
	if budget.Attempts(alice, "vendor") != 1 || budget.Attempts(bob, "vendor") != 0 {
		t.Fatal("same-account counters were mixed")
	}
	if err := budget.Dispatch(bob, policy); !errors.Is(err, ErrDenied) {
		t.Fatal("account switch reset root", err)
	}
	if consumed, _ := budget.Counts("vendor"); consumed != 2 {
		t.Fatal("reservation charged twice", consumed)
	}
}

func TestAvailabilityKeepsIndependentModelWindowsAndAccountGenerations(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var availability Availability
	refusal := Refusal{Classification: Classification{Class: Quota, Scope: "model", Rule: "free"}, Model: "free", NoEffect: true, ObservedAt: now, Windows: []Window{
		{ID: "free", Models: []string{"free"}, ResetAt: now.Add(time.Hour).Format(time.RFC3339)},
		{ID: "shared", NotModels: []string{"paid"}, ResetAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
	}}
	availability.Remember("alice-generation1", refusal)
	if blocked, ok := availability.Blocked("alice-generation1", "free", now); !ok || !blocked.NextAllowed.Equal(now.Add(2*time.Hour)) {
		t.Fatal(blocked, ok)
	}
	for _, selection := range []struct{ account, model string }{{"alice-generation1", "paid"}, {"alice-generation2", "free"}, {"bob-generation1", "free"}} {
		if _, ok := availability.Blocked(selection.account, selection.model, now); ok {
			t.Fatal("unrelated availability poisoned", selection)
		}
	}
	refusal.Windows = []Window{{ID: "unknown", Models: []string{"paid"}}}
	availability.Remember("alice-generation1", refusal)
	if _, ok := availability.Blocked("alice-generation1", "paid", now); ok {
		t.Fatal("unknown reset became guessed cooldown")
	}
	if _, ok := availability.Blocked("alice-generation1", "free", now.Add(2*time.Hour)); ok {
		t.Fatal("expired window retained")
	}
}
