package requestpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

type Options struct {
	MaxRetries      *int               `json:"maxRetries,omitempty"`
	MaxTotalRetries *int               `json:"maxTotalRetries,omitempty"`
	BaseDelayMS     *int               `json:"baseDelayMs,omitempty"`
	MaxDelayMS      *int               `json:"maxDelayMs,omitempty"`
	MaxElapsedMS    *int               `json:"maxElapsedMs,omitempty"`
	JitterPercent   *int               `json:"jitterPercent,omitempty"`
	Rules           []Rule             `json:"rules,omitempty"`
	ShortWaitMS     *int               `json:"shortWaitMs,omitempty"`
	Models          map[string]Options `json:"models,omitempty"`
	Windows         map[string]Options `json:"windows,omitempty"`
}

type Config struct {
	Defaults  Options            `json:"defaults"`
	Providers map[string]Options `json:"providers,omitempty"`
}

func (config *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var value plain
	if len(data) > 128*1024 {
		return fmt.Errorf("request policy exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*config = Config(value)
	return config.Validate()
}

type Policy struct {
	Provider                        string
	MaxRetries                      *int
	MaxTotalRetries                 int
	BaseDelay, MaxDelay, MaxElapsed time.Duration
	JitterPercent                   int
	Rules                           []Rule
	ShortWait                       time.Duration
}

func Defaults() Policy {
	return Policy{MaxTotalRetries: 64, BaseDelay: time.Second, MaxDelay: 8 * time.Second, MaxElapsed: 30 * time.Minute, ShortWait: 8 * time.Second}
}

func (options Options) validate() error {
	for _, window := range options.Windows {
		if len(window.Rules) > 0 {
			return fmt.Errorf("window overrides select limits, not response classification")
		}
	}
	for _, overrides := range []map[string]Options{options.Models, options.Windows} {
		if len(overrides) > 128 {
			return fmt.Errorf("too many request policy overrides")
		}
		for identity, override := range overrides {
			if identity == "" || len(identity) > 300 || len(override.Models) > 0 || len(override.Windows) > 0 {
				return fmt.Errorf("invalid scoped request policy override")
			}
			if err := override.validate(); err != nil {
				return err
			}
		}
	}
	for _, bound := range []struct {
		value            *int
		minimum, maximum int
	}{
		{options.MaxRetries, 0, 64}, {options.MaxTotalRetries, 0, 256}, {options.BaseDelayMS, 0, 60000},
		{options.MaxDelayMS, 0, 604800000}, {options.MaxElapsedMS, 1, 604800000}, {options.ShortWaitMS, 0, 300000}, {options.JitterPercent, 0, 100},
	} {
		if bound.value != nil && (*bound.value < bound.minimum || *bound.value > bound.maximum) {
			return fmt.Errorf("request policy value out of range")
		}
	}
	if len(options.Rules) > 64 {
		return fmt.Errorf("too many request policy rules")
	}
	seen := map[string]bool{}
	for _, rule := range options.Rules {
		if seen[rule.ID] {
			return fmt.Errorf("duplicate request policy rule")
		}
		seen[rule.ID] = true
		if err := rule.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (config *Config) Validate() error {
	if config == nil {
		return nil
	}
	if err := config.Defaults.validate(); err != nil {
		return err
	}
	if len(config.Providers) > 256 {
		return fmt.Errorf("too many provider policies")
	}
	for id, options := range config.Providers {
		if id == "" || len(id) > 256 {
			return fmt.Errorf("invalid provider policy identity")
		}
		if err := options.validate(); err != nil {
			return err
		}
	}
	return nil
}

func apply(policy *Policy, options Options) {
	if options.ShortWaitMS != nil {
		policy.ShortWait = time.Duration(*options.ShortWaitMS) * time.Millisecond
	}
	if options.MaxRetries != nil {
		limit := *options.MaxRetries
		policy.MaxRetries = &limit
	}
	if options.MaxTotalRetries != nil {
		policy.MaxTotalRetries = *options.MaxTotalRetries
	}
	if options.BaseDelayMS != nil {
		policy.BaseDelay = time.Duration(*options.BaseDelayMS) * time.Millisecond
	}
	if options.MaxDelayMS != nil {
		policy.MaxDelay = time.Duration(*options.MaxDelayMS) * time.Millisecond
	}
	if options.MaxElapsedMS != nil {
		policy.MaxElapsed = time.Duration(*options.MaxElapsedMS) * time.Millisecond
	}
	if options.JitterPercent != nil {
		policy.JitterPercent = *options.JitterPercent
	}
	policy.Rules = append(append([]Rule(nil), options.Rules...), policy.Rules...)
}

func Resolve(config *Config, provider string, base Policy) (Policy, error) {
	if err := config.Validate(); err != nil {
		return Policy{}, err
	}
	if config != nil {
		apply(&base, config.Defaults)
		apply(&base, config.Providers[provider])
	}
	base.Provider = provider
	return base, nil
}

// Safety is independent of classification. Configured rules cannot supply it.
// Accepted concerns the business step being repeated, not an earlier paid step
// in a handler whose separately identified safe read is now being retried.
type Safety struct{ NoEffect, Accepted, Uncertain, OutputStarted bool }

type Decision struct {
	Classification
	Retry       bool
	Delay       time.Duration
	Reason      string
	NextAllowed time.Time
	Action      string
}

func (policy Policy) Decide(facts Facts, fallback Classification, safety Safety, again int, now time.Time, random float64) Decision {
	decision := Decision{Classification: policy.Classify(facts, fallback), Reason: "not-retryable"}
	var minimum time.Duration
	if facts.Complete && facts.Validate() == nil {
		minimum = RetryMinimum(facts, now)
		if minimum > 0 {
			decision.NextAllowed = now.Add(minimum).UTC()
		}
	}
	if !safety.NoEffect || safety.Accepted || safety.Uncertain || safety.OutputStarted || again < 0 {
		decision.Reason = "unsafe"
		return decision
	}
	limit := 2
	switch decision.Class {
	case Rate:
		limit = 3
	case Outage:
	default:
		return decision
	}
	if policy.MaxRetries != nil {
		limit = *policy.MaxRetries
	}
	if again >= limit {
		decision.Reason = "attempt-limit"
		return decision
	}
	decision.Delay = policy.BaseDelay
	for exponent := 0; exponent < again; exponent++ {
		if decision.Delay > policy.MaxDelay/2 {
			decision.Delay = policy.MaxDelay
			break
		}
		decision.Delay *= 2
	}
	decision.Delay = min(decision.Delay, policy.MaxDelay)
	// Nonnegative jitter avoids ever undercutting the base schedule.
	if random < 0 || random > 1 {
		decision.Reason = "invalid-random"
		return decision
	}
	decision.Delay += time.Duration(float64(decision.Delay) * float64(policy.JitterPercent) / 100 * random)
	decision.Delay = min(decision.Delay, policy.MaxDelay)
	decision.Delay = max(decision.Delay, minimum)
	decision.NextAllowed = now.Add(decision.Delay).UTC()
	if decision.Delay > policy.MaxDelay {
		decision.Reason = "delay-limit"
		return decision
	}
	decision.Retry = true
	decision.Reason = "retry"
	return decision
}

var ErrDenied = errors.New("request retry budget exhausted")

type Budget struct {
	mu         sync.Mutex
	started    time.Time
	used       int
	byProvider map[string]int
	root       *operation
	now        func() time.Time
	random     func() float64
	config     *Config
}

func NewBudget() *Budget { return newBudget(time.Now, rand.Float64) }
func newBudget(now func() time.Time, random func() float64) *Budget {
	budget := &Budget{started: now(), now: now, random: random, byProvider: make(map[string]int)}
	budget.root = newOperation(budget)
	return budget
}

type budgetKey struct{}

func WithBudget(ctx context.Context) context.Context {
	if FromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, NewBudget())
}
func WithConfig(ctx context.Context, config *Config) context.Context {
	if FromContext(ctx) != nil {
		return ctx
	}
	budget := NewBudget()
	// Snapshot configuration so a reload cannot replenish this request's budget.
	if config != nil {
		raw, err := json.Marshal(config)
		var clone Config
		if err != nil || json.Unmarshal(raw, &clone) != nil {
			zero := 0
			budget.config = &Config{Defaults: Options{MaxTotalRetries: &zero, MaxRetries: &zero}}
		} else {
			budget.config = &clone
		}
	}
	return context.WithValue(ctx, budgetKey{}, budget)
}
func (budget *Budget) Policy(provider string, base Policy) (Policy, error) {
	return budget.PolicyFor(provider, "", "", base)
}
func (budget *Budget) PolicyFor(provider, model, window string, base Policy) (Policy, error) {
	return ResolveFor(budget.config, provider, model, window, base)
}
func ResolveFor(config *Config, provider, model, window string, base Policy) (Policy, error) {
	policy, err := Resolve(config, provider, base)
	if err != nil || config == nil {
		return policy, err
	}
	apply(&policy, config.Defaults.Models[model])
	apply(&policy, config.Providers[provider].Models[model])
	apply(&policy, config.Defaults.Windows[window])
	apply(&policy, config.Providers[provider].Windows[window])
	return policy, nil
}
func FromContext(ctx context.Context) *Budget {
	budget, _ := ctx.Value(budgetKey{}).(*Budget)
	return budget
}

// Admit atomically consumes the shared root budget. Cancellation does not give
// it back: a nested caller cannot replenish an outer retry by abandoning a wait.
func (budget *Budget) Admit(ctx context.Context, policy Policy, decision Decision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.admitLocked(ctx, policy, decision)
}

func (budget *Budget) admitLocked(ctx context.Context, policy Policy, decision Decision) error {
	if err := budget.checkLocked(ctx, policy, decision); err != nil {
		return err
	}
	budget.used++
	budget.byProvider[policy.Provider]++
	return nil
}

func (budget *Budget) checkLocked(ctx context.Context, policy Policy, decision Decision) error {
	rootLimit := Defaults().MaxTotalRetries
	rootElapsed := Defaults().MaxElapsed
	if budget.config != nil && budget.config.Defaults.MaxElapsedMS != nil {
		rootElapsed = time.Duration(*budget.config.Defaults.MaxElapsedMS) * time.Millisecond
	}
	if budget.config != nil && budget.config.Defaults.MaxTotalRetries != nil {
		rootLimit = *budget.config.Defaults.MaxTotalRetries
	}
	if !decision.Retry || decision.Delay < 0 || decision.Delay > policy.MaxDelay || budget.used >= min(rootLimit, policy.MaxTotalRetries) || budget.now().Sub(budget.started)+decision.Delay >= min(rootElapsed, policy.MaxElapsed) {
		return ErrDenied
	}
	if end, ok := ctx.Deadline(); ok && !budget.now().Add(decision.Delay).Before(end) {
		return ErrDenied
	}
	return nil
}

func (budget *Budget) Decide(policy Policy, facts Facts, config Classification, safety Safety, again int) Decision {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return policy.Decide(facts, config, safety, again, budget.now(), budget.random())
}

// Wait owns cancellation and scheduling. A gateway may supply its existing
// keepalive-aware wait; modules never receive a sleep interval to implement.
func (budget *Budget) Wait(ctx context.Context, policy Policy, decision Decision, wait func(context.Context, time.Duration) error) error {
	if err := budget.Admit(ctx, policy, decision); err != nil {
		return err
	}
	if wait != nil {
		return wait(ctx, decision.Delay)
	}
	return Wait(ctx, decision.Delay)
}

func Wait(ctx context.Context, decision time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(decision)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
