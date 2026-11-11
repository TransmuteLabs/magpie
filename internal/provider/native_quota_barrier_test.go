package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A reset may be observed while an older usage read is still in flight.
// Its waiter and the persisted fallback must both follow the later read.
func TestResetQuotaBarrierWaitsForNewRead(t *testing.T) {
	isolateRefreshes(t)
	signIn(t)
	oldArrived, newArrived := make(chan struct{}), make(chan struct{})
	oldRelease, newRelease := make(chan struct{}), make(chan struct{})
	var oldOnce, newOnce sync.Once
	var mu sync.Mutex
	var readings sync.WaitGroup
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			close(oldArrived)
			<-oldRelease
		case 2:
			close(newArrived)
			<-newRelease
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		used := 20
		if n == 2 {
			used = 80
		}
		fmt.Fprintf(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000}}}`, used)
	}))
	base := CodexBase
	CodexBase = srv.URL + "/backend-api/codex"
	t.Cleanup(func() {
		oldOnce.Do(func() { close(oldRelease) })
		newOnce.Do(func() { close(newRelease) })
		srv.Close()
		readings.Wait()
		CodexBase = base
		quotaReadBarriers.Lock()
		delete(quotaReadBarriers.m, "codex/me@example.com")
		quotaReadBarriers.Unlock()
		loginUsageCache.Lock()
		delete(loginUsageCache.generation, "codex/me@example.com")
		delete(loginUsageCache.pending, "codex/me@example.com")
		delete(loginUsageCache.m, "codex/me@example.com")
		loginUsageCache.Unlock()
	})
	ctx, cancel := context.WithTimeout(Asked(context.Background()), 10*time.Second)
	defer cancel()
	l := Login{Agent: "codex", User: "me@example.com", Active: true}
	oldDone, newDone := make(chan loginUsageEntry, 1), make(chan loginUsageEntry, 1)
	readings.Add(1)
	go func() { defer readings.Done(); oldDone <- cachedLoginReading(ctx, l) }()
	waitRefresh(t, oldArrived)
	loginUsageCache.Lock()
	oldPending := loginUsageCache.pending["codex/me@example.com"]
	loginUsageCache.Unlock()
	if oldPending == nil {
		t.Fatal("older read not found")
	}
	resetQuotaReadBarrier(l)
	readings.Add(1)
	go func() { defer readings.Done(); newDone <- cachedLoginReading(ctx, l) }()
	waitRefresh(t, newArrived)
	oldOnce.Do(func() { close(oldRelease) })
	// Confirm the older read has actually finished, not merely been released.
	waitRefresh(t, oldPending.done)
	loginUsageCache.Lock()
	newPending := loginUsageCache.pending["codex/me@example.com"]
	loginUsageCache.Unlock()
	if newPending == nil {
		t.Fatal("fresh read was removed by an older completion")
	}
	select {
	case got := <-oldDone:
		t.Fatalf("old read satisfied a post-reset waiter before the fresh read: %+v", got)
	default:
	}
	newOnce.Do(func() { close(newRelease) })
	for _, result := range []<-chan loginUsageEntry{oldDone, newDone} {
		select {
		case got := <-result:
			if got.q.Error != "" || len(got.q.Windows) != 1 || got.q.Windows[0].Used != 80 {
				t.Fatalf("post-reset reading = %+v, want fresh 80%%", got.q)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	lastQuotas.Lock()
	lastQuotas.m, lastQuotas.loaded = nil, false
	lastQuotas.Unlock()
	q := keepLast(SubscriptionQuota{Provider: "codex", Error: "service unavailable"}, l.User)
	if len(q.Windows) != 1 || q.Windows[0].Used != 80 || q.AsOf == nil {
		t.Fatalf("persisted fallback = %+v, want the fresh reading", q)
	}
}
