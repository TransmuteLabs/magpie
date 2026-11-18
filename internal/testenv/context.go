package testenv

import (
	"context"
	"testing"
	"time"
)

func DeadlineContext(t *testing.T, join time.Duration) context.Context {
	t.Helper()
	deadline, bounded := t.Deadline()
	ctx, cancel := deadlineContext(t.Context(), deadline, bounded, join)
	t.Cleanup(cancel)
	return ctx
}

func deadlineContext(parent context.Context, deadline time.Time, bounded bool, join time.Duration) (context.Context, context.CancelFunc) {
	if !bounded {
		return context.WithCancel(parent)
	}
	reserve := max(time.Until(deadline), 0)/10 + join
	return context.WithDeadline(parent, deadline.Add(-reserve))
}
