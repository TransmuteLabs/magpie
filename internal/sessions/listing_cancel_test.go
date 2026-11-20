package sessions

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestFindStopsBeforeParsingCanceledListing(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var entered bool
	var parses atomic.Int32
	filesHook = func() {
		entered = true
		cancel()
	}
	parseHook = func() { parses.Add(1) }
	t.Cleanup(func() { filesHook, parseHook = nil, nil })

	if row, found := FindContext(ctx, "codex", "missing"); found {
		t.Fatalf("canceled lookup returned %+v", row)
	}
	if !entered || parses.Load() != 0 {
		t.Fatalf("discovery entered %v, parsed %d files after cancellation", entered, parses.Load())
	}
	filesHook, parseHook = nil, nil
	if rows := List(0); len(rows) != 2 {
		t.Fatalf("next listing did not recover the two fixture sessions: %+v", rows)
	}
}
