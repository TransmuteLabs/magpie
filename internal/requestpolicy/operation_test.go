package requestpolicy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChildOperationsShareRetriesNotFirstDispatches(t *testing.T) {
	total := 1
	ctx := WithConfig(t.Context(), &Config{Defaults: Options{MaxTotalRetries: &total}})
	budget := FromContext(ctx)
	policy, err := budget.Policy("vendor", Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var children []context.Context
	for range 300 {
		child := WithSelection(WithChildOperation(ctx), "vendor/alice/free", "free")
		if FromContext(child) != budget || !budget.CanDispatch(child, policy) {
			t.Fatal("child lost its root or first dispatch")
		}
		if err := budget.Dispatch(child, policy); err != nil {
			t.Fatal("independent first dispatch charged as retry", err)
		}
		if budget.Attempts(child, "vendor") != 0 {
			t.Fatal("sibling dispatch counted as child retry")
		}
		children = append(children, child)
	}
	if used, _ := budget.Counts("vendor"); used != 0 {
		t.Fatal("first child operations consumed root retries", used)
	}
	if err := budget.Reserve(children[0], policy, Decision{Retry: true}); err != nil {
		t.Fatal(err)
	}
	if err := budget.Dispatch(children[0], policy); err != nil {
		t.Fatal("reservation charged twice", err)
	}
	for _, child := range children {
		if budget.CanDispatch(child, policy) || !errors.Is(budget.Dispatch(child, policy), ErrDenied) {
			t.Fatal("child replenished the consumed root budget")
		}
	}
	if err := budget.Dispatch(ctx, policy); err != nil {
		t.Fatal("children spent the parent's first business operation", err)
	}
	if err := budget.Dispatch(WithSelection(ctx, "vendor/bob/paid"), policy); !errors.Is(err, ErrDenied) {
		t.Fatal("parent fallback escaped the shared root", err)
	}
	if used, selected := budget.Counts("vendor"); used != 1 || selected != 1 {
		t.Fatal("root/provider retry counts changed", used, selected)
	}
}

func TestChildReservationsAndSelectionStayWithTheirOperation(t *testing.T) {
	total := 1
	parent := WithRoutePermit(WithSelection(WithConfig(t.Context(), &Config{Defaults: Options{MaxTotalRetries: &total}}), "alice", "free"))
	budget := FromContext(parent)
	policy, err := budget.Policy("vendor", Defaults())
	if err != nil {
		t.Fatal(err)
	}
	child := WithChildOperation(parent)
	if HasSelection(child) || SelectedModel(child) != "" || ConsumeRoutePermit(child) || !ConsumeRoutePermit(parent) {
		t.Fatal("child inherited its parent's selected execution permit")
	}
	first := WithSelection(child, "alice", "free")
	second := WithSelection(WithChildOperation(parent), "alice", "free")
	if err := budget.Dispatch(first, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Reserve(first, policy, Decision{Retry: true}); err != nil {
		t.Fatal(err)
	}
	if err := budget.Dispatch(second, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Dispatch(second, policy); !errors.Is(err, ErrDenied) {
		t.Fatal("another operation stole the reservation", err)
	}
	if err := budget.Dispatch(first, policy); err != nil {
		t.Fatal("reservation was not retained by its owner", err)
	}
	if budget.Attempts(first, "vendor") != 1 || budget.Attempts(second, "vendor") != 0 {
		t.Fatal("same-selection attempts leaked across operations")
	}
}

func TestConcurrentChildOperationsShareOneRootLimit(t *testing.T) {
	total := 3
	ctx := WithConfig(t.Context(), &Config{Defaults: Options{MaxTotalRetries: &total}})
	budget := FromContext(ctx)
	policy, err := budget.Policy("vendor", Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var firsts, repeats atomic.Int32
	var workers sync.WaitGroup
	for range 50 {
		child := WithSelection(WithChildOperation(ctx), "same-account", "same-model")
		workers.Go(func() {
			if budget.Dispatch(child, policy) == nil {
				firsts.Add(1)
			}
			if budget.Dispatch(child, policy) == nil {
				repeats.Add(1)
			}
		})
	}
	workers.Wait()
	if firsts.Load() != 50 || repeats.Load() != int32(total) {
		t.Fatal("concurrent first operations or retry limit changed", firsts.Load(), repeats.Load())
	}
}

func TestChildCancellationDoesNotRefundOrRestartElapsedBudget(t *testing.T) {
	now := time.Now()
	budget := newBudget(func() time.Time { return now }, func() float64 { return 0 })
	ctx := context.WithValue(t.Context(), budgetKey{}, budget)
	policy := Defaults()
	policy.MaxTotalRetries, policy.MaxElapsed = 1, 10*time.Second
	first, cancel := context.WithCancel(WithChildOperation(ctx))
	if err := budget.Dispatch(first, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Reserve(first, policy, Decision{Retry: true}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := budget.Dispatch(first, policy); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled child dispatched", err)
	}
	second := WithChildOperation(ctx)
	if err := budget.Dispatch(second, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Reserve(second, policy, Decision{Retry: true}); !errors.Is(err, ErrDenied) {
		t.Fatal("cancellation refunded root permit", err)
	}
	budget = newBudget(func() time.Time { return now }, func() float64 { return 0 })
	ctx = context.WithValue(t.Context(), budgetKey{}, budget)
	now = now.Add(9 * time.Second)
	child := WithChildOperation(ctx)
	if err := budget.Dispatch(child, policy); err != nil {
		t.Fatal(err)
	}
	if err := budget.Reserve(child, policy, Decision{Retry: true, Delay: time.Second}); !errors.Is(err, ErrDenied) {
		t.Fatal("child restarted the root elapsed clock", err)
	}
}
