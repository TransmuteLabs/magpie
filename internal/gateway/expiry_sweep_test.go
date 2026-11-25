package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
)

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func TestGatewayRetentionStoresSweepExpiredIdleEntries(t *testing.T) {
	const shortTTL = 25 * time.Millisecond

	t.Run("prevStore", func(t *testing.T) {
		st := newPrevStore(1<<20, shortTTL)
		st.put(&prevTurn{
			key:   prevKey{scope: "s", id: "r1"},
			items: []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"hi"}`)},
			own:   1024,
		})
		waitUntil(t, time.Second, func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.size == 0 && len(st.m) == 0 && st.order.Len() == 0 && st.sweep.timer == nil
		})
	})

	t.Run("remoteSentStore", func(t *testing.T) {
		st := newRemoteSentStore()
		st.ttl = shortTTL
		st.put(&remoteSent{
			key:   "route|scope",
			input: []itemSum{sha256.Sum256([]byte("x"))},
			id:    "resp_1",
		}, nil)
		waitUntil(t, time.Second, func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.size == 0 && len(st.m) == 0 && st.order.Len() == 0 && st.sweep.timer == nil
		})
	})

	t.Run("sentBodies", func(t *testing.T) {
		st := newSentBodies()
		st.ttl = shortTTL
		body := bytes.Repeat([]byte("a"), 2048)
		st.put("route-a", &sentDict{sum: sha256.Sum256(body), body: body, at: anchorsOf(body), when: time.Now()})
		waitUntil(t, time.Second, func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.size == 0 && len(st.routes) == 0 && st.sweep.timer == nil && st.speaks["route-a"]
		})
	})

	t.Run("keptBodies", func(t *testing.T) {
		st := newKeptBodies()
		st.ttl = shortTTL
		st.put("scope-a", bytes.Repeat([]byte("b"), 2048))
		waitUntil(t, time.Second, func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.size == 0 && len(st.scopes) == 0 && st.lru.Len() == 0 && st.sweep.timer == nil
		})
	})

	t.Run("keptImageStore", func(t *testing.T) {
		st := newKeptImageStore(1<<20, shortTTL)
		id := st.put(bytes.Repeat([]byte("i"), 2048))
		if _, ok := st.get(id); !ok {
			t.Fatal("fresh image missing before TTL")
		}
		waitUntil(t, time.Second, func() bool {
			st.Lock()
			defer st.Unlock()
			return st.size == 0 && len(st.byID) == 0 && len(st.bySum) == 0 && st.lru.Len() == 0 && st.sweep.timer == nil
		})
	})

	t.Run("remoteSentStoreEarlierRefusedDeadline", func(t *testing.T) {
		st := newRemoteSentStore()
		st.ttl = time.Hour
		st.refuseTTL = shortTTL
		t.Cleanup(func() {
			st.mu.Lock()
			st.sweep.stop()
			st.mu.Unlock()
		})
		st.put(&remoteSent{
			key:   "route|scope",
			input: []itemSum{sha256.Sum256([]byte("long"))},
			id:    "resp_long",
		}, nil)
		st.refuse("route-refused")
		waitUntil(t, time.Second, func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return len(st.refused) == 0 && st.order.Len() == 1 && st.sweep.timer != nil
		})
	})

	t.Run("keptImageStoreRefreshReschedulesSweep", func(t *testing.T) {
		st := newKeptImageStore(1<<20, 60*time.Millisecond)
		id := st.put(bytes.Repeat([]byte("r"), 2048))
		time.Sleep(35 * time.Millisecond)
		if _, ok := st.get(id); !ok {
			t.Fatal("entry expired before refreshed TTL")
		}
		time.Sleep(35 * time.Millisecond)
		if _, ok := st.get(id); !ok {
			t.Fatal("refreshed entry expired at initial TTL")
		}
		waitUntil(t, time.Second, func() bool {
			st.Lock()
			defer st.Unlock()
			return st.size == 0 && len(st.byID) == 0 && st.sweep.timer == nil
		})
	})

	t.Run("archivedBodyCache", func(t *testing.T) {
		cache := archivedBodyCache{ttl: shortTTL, now: time.Now}
		cache.Lock()
		cache.id, cache.body, cache.when = 42, bytes.Repeat([]byte("z"), 1024), cache.now()
		cache.sweep.arm(cache.ttl, cache.sweepExpired)
		cache.Unlock()
		waitUntil(t, time.Second, func() bool {
			cache.Lock()
			defer cache.Unlock()
			return cache.body == nil && cache.id == 0 && cache.sweep.timer == nil
		})
	})
}
