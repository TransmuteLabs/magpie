package testenv

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestDeadlineContextReservesJoinBeforeHardTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hard := time.Now().Add(62 * time.Second)
		ctx, cancel := deadlineContext(t.Context(), hard, true, 2*time.Second)
		defer cancel()
		want := hard.Add(-6200*time.Millisecond - 2*time.Second)
		deadline, bounded := ctx.Deadline()
		if !bounded || !deadline.Equal(want) {
			t.Fatalf("compiler deadline %v, bounded=%v; want %v", deadline, bounded, want)
		}
		<-ctx.Done()
		if ctx.Err() != context.DeadlineExceeded || !time.Now().Equal(want) {
			t.Fatalf("compiler stopped at %v with %v, want %v before %v", time.Now(), ctx.Err(), want, hard)
		}
	})
}

func TestDeadlineContextKeepsParentCancellationAndValues(t *testing.T) {
	type marker struct{}
	for _, bounded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbounded", true: "bounded"}[bounded], func(t *testing.T) {
			parent, cancelParent := context.WithCancel(context.WithValue(t.Context(), marker{}, "compiler"))
			defer cancelParent()
			ctx, cancel := deadlineContext(parent, time.Now().Add(time.Minute), bounded, 2*time.Second)
			defer cancel()
			if ctx.Value(marker{}) != "compiler" {
				t.Fatal("compiler context lost caller values")
			}
			if _, got := ctx.Deadline(); got != bounded {
				t.Fatalf("bounded=%v, want %v", got, bounded)
			}
			cancelParent()
			if ctx.Err() != context.Canceled {
				t.Fatalf("compiler ignored caller cancellation: %v", ctx.Err())
			}
		})
	}
}

func TestDeadlineContextKeepsEarlierParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		earlier := time.Now().Add(time.Second)
		parent, cancelParent := context.WithDeadline(t.Context(), earlier)
		defer cancelParent()
		ctx, cancel := deadlineContext(parent, time.Now().Add(time.Minute), true, 2*time.Second)
		defer cancel()
		if deadline, bounded := ctx.Deadline(); !bounded || !deadline.Equal(earlier) {
			t.Fatalf("compiler extended caller deadline: %v, bounded=%v", deadline, bounded)
		}
		<-ctx.Done()
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("compiler ignored caller deadline: %v", ctx.Err())
		}
	})
}

func TestDeadlineContextRejectsExhaustedJoinBudget(t *testing.T) {
	for _, remaining := range []time.Duration{time.Second, -time.Second} {
		ctx, cancel := deadlineContext(t.Context(), time.Now().Add(remaining), true, 2*time.Second)
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("compiler could start with %v remaining: %v", remaining, ctx.Err())
		}
		cancel()
	}
}

func TestDeadlineContextCancelAndCleanup(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		ctx, cancel := deadlineContext(t.Context(), time.Now().Add(time.Minute), bounded, 2*time.Second)
		cancel()
		if ctx.Err() != context.Canceled {
			t.Fatalf("bounded=%v compiler cleanup did not cancel: %v", bounded, ctx.Err())
		}
	}
	var child context.Context
	t.Run("cleanup", func(t *testing.T) {
		child = DeadlineContext(t, 2*time.Second)
		if child.Err() != nil {
			t.Fatal(child.Err())
		}
	})
	if child.Err() != context.Canceled {
		t.Fatalf("compiler survived test cleanup: %v", child.Err())
	}
}

func TestDeadlineContextKeepsFreshSemanticMaximum(t *testing.T) {
	for _, maximum := range []time.Duration{time.Minute, 2 * time.Minute} {
		synctest.Test(t, func(t *testing.T) {
			hard := time.Now().Add(10 * time.Minute)
			time.Sleep(65 * time.Second)
			parent, stop := deadlineContext(t.Context(), hard, true, 2*time.Second)
			defer stop()
			ctx, cancel := context.WithTimeout(parent, maximum)
			defer cancel()
			want := time.Now().Add(maximum)
			if got, bounded := ctx.Deadline(); !bounded || !got.Equal(want) {
				t.Fatalf("fresh semantic maximum %v became %v, want %v", maximum, got, want)
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		hard := time.Now().Add(62 * time.Second)
		time.Sleep(40 * time.Second)
		parent, stop := deadlineContext(t.Context(), hard, true, 2*time.Second)
		defer stop()
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		want := hard.Add(-2200*time.Millisecond - 2*time.Second)
		if got, bounded := ctx.Deadline(); !bounded || !got.Equal(want) {
			t.Fatalf("semantic child extended enclosing deadline: %v, want %v", got, want)
		}
	})
}
