package requestpolicy

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	ActionWait     = "wait"
	ActionNext     = "next-account"
	ActionTerminal = "terminal"
)

// Disposition separates the response classification from what the host may do
// next. A known account/model refusal never supplies replay evidence by itself.
func (policy Policy) Disposition(facts Facts, classification Classification, safety Safety, again int, now time.Time, random float64) Decision {
	classification = policy.Classify(facts, classification)
	decision := Decision{Classification: classification, Action: ActionTerminal, Reason: "not-retryable"}
	if facts.Validate() != nil || !facts.Complete {
		return decision
	}
	minimum := RetryMinimum(facts, now)
	if minimum > 0 {
		decision.NextAllowed = now.Add(minimum).UTC()
	}
	if !safety.NoEffect || safety.Accepted || safety.Uncertain || safety.OutputStarted {
		decision.Reason = "unsafe"
		return decision
	}
	if classification.Class == Unknown || classification.Scope == "unknown" || classification.Class == Content || classification.Class == Request {
		return decision
	}
	// An auth refusal can move only to another eligible account. API 1.12 never
	// refreshes credentials, and a request-scoped refusal cannot enumerate a pool.
	if classification.Class == Auth || classification.Class == Quota && minimum == 0 {
		if classification.Scope == "account" || classification.Scope == "model" {
			decision.Action = ActionNext
			decision.Reason = "selected-scope-refused"
		}
		return decision
	}
	if classification.Class != Rate && classification.Class != Outage && classification.Class != Quota {
		return decision
	}
	if classification.Class == Quota && classification.Scope == "request" {
		return decision
	}
	scheduler := policy
	scheduler.Rules = nil
	scheduledClass := classification
	if scheduledClass.Class == Quota {
		scheduledClass.Class = Rate
	}
	decision = scheduler.Decide(facts, scheduledClass, safety, again, now, random)
	decision.Classification = classification
	decision.Action = ActionTerminal
	if !decision.Retry {
		decision.Action = ActionNext
		return decision
	}
	if decision.Delay > policy.ShortWait {
		decision.Retry = false
		decision.Action = ActionNext
		decision.Reason = "short-wait-exceeded"
		return decision
	}
	decision.Action = ActionWait
	return decision
}

func (budget *Budget) Disposition(policy Policy, facts Facts, classification Classification, safety Safety, again int) Decision {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return policy.Disposition(facts, classification, safety, again, budget.now(), budget.random())
}

// Counts are per logical request. Provider counts include additional dispatches
// on sibling accounts; they are independent of cooldown and eligibility.
func (budget *Budget) Counts(provider string) (total, selected int) {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.used, budget.byProvider[provider]
}

// Reserve spends a permit before waiting. Dispatch consumes it once instead of
// charging twice. Cancellation never returns a reservation to the root budget.
func (budget *Budget) Reserve(ctx context.Context, policy Policy, decision Decision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if err := budget.admitLocked(ctx, policy, decision); err != nil {
		return err
	}
	budget.operationOf(ctx).reserved[selectionOf(ctx, policy.Provider)]++
	return nil
}

func (budget *Budget) Dispatch(ctx context.Context, policy Policy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	operation := budget.operationOf(ctx)
	selection := selectionOf(ctx, policy.Provider)
	if operation.reserved[selection] > 0 {
		operation.reserved[selection]--
		operation.executions[selection]++
		operation.dispatched = true
		return nil
	}
	if !operation.dispatched {
		operation.dispatched = true
		operation.executions[selection]++
		return nil
	}
	if err := budget.admitLocked(ctx, policy, Decision{Retry: true}); err != nil {
		return err
	}
	operation.executions[selection]++
	return nil
}

func (budget *Budget) CanDispatch(ctx context.Context, policy Policy) bool {
	if ctx.Err() != nil {
		return false
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	operation := budget.operationOf(ctx)
	return !operation.dispatched || operation.reserved[selectionOf(ctx, policy.Provider)] > 0 ||
		budget.checkLocked(ctx, policy, Decision{Retry: true}) == nil
}

type operation struct {
	budget     *Budget
	dispatched bool
	reserved   map[string]int
	executions map[string]int
}

func newOperation(budget *Budget) *operation {
	return &operation{budget: budget, reserved: make(map[string]int), executions: make(map[string]int)}
}

type operationKey struct{}

func WithChildOperation(ctx context.Context) context.Context {
	budget := FromContext(ctx)
	if budget == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, operationKey{}, newOperation(budget))
	ctx = context.WithValue(ctx, routePermitKey{}, (*atomic.Bool)(nil))
	return WithSelection(ctx, "")
}

func (budget *Budget) operationOf(ctx context.Context) *operation {
	if selected, _ := ctx.Value(operationKey{}).(*operation); selected != nil && selected.budget == budget {
		return selected
	}
	return budget.root
}

type routePermitKey struct{}

// A route admission covers its first nested SDK or builtin transport dispatch.
// Subsequent dispatches cannot reuse this context marker.
func WithRoutePermit(ctx context.Context) context.Context {
	return context.WithValue(ctx, routePermitKey{}, new(atomic.Bool))
}
func ConsumeRoutePermit(ctx context.Context) bool {
	permit, _ := ctx.Value(routePermitKey{}).(*atomic.Bool)
	return permit != nil && permit.CompareAndSwap(false, true)
}

type selectionKey struct{}
type selectedRequest struct{ identity, model string }

func WithSelection(ctx context.Context, identity string, models ...string) context.Context {
	selected := selectedRequest{identity: identity}
	if len(models) > 0 {
		selected.model = models[0]
	}
	return context.WithValue(ctx, selectionKey{}, selected)
}
func SelectedModel(ctx context.Context) string {
	selected, _ := ctx.Value(selectionKey{}).(selectedRequest)
	return selected.model
}
func selectionOf(ctx context.Context, fallback string) string {
	selected, _ := ctx.Value(selectionKey{}).(selectedRequest)
	if selected.identity == "" {
		return fallback
	}
	return selected.identity
}
func HasSelection(ctx context.Context) bool { return selectionOf(ctx, "") != "" }
func (budget *Budget) Attempts(ctx context.Context, provider string) int {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return max(0, budget.operationOf(ctx).executions[selectionOf(ctx, provider)]-1)
}
