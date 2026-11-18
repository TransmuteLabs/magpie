package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
)

func TestLaneClaimUnusedReturnsWithoutCompletedTurn(t *testing.T) {
	var slots lanes
	claim, ok := slots.tryClaim("account", 1)
	if !ok {
		t.Fatal("initial claim denied")
	}
	ctx, cancel := context.WithCancel(t.Context())
	var waiter sync.WaitGroup
	defer func() {
		cancel()
		claim.unused()
		waiter.Wait()
	}()
	result := make(chan *laneClaim, 1)
	waiter.Go(func() {
		next, _, err := slots.takeClaim(ctx, "account", 1, 0, 0, nil)
		if err != nil {
			result <- nil
			return
		}
		result <- next
	})
	waitFor(t, "ordinary second claim queued", func() bool {
		slots.mu.Lock()
		defer slots.mu.Unlock()
		return len(slots.m["account"].queue) == 1
	})
	claim.unused()
	claim.complete()
	var next *laneClaim
	select {
	case next = <-result:
	case <-time.After(time.Second):
		t.Fatal("unused claim did not wake its waiter")
	}
	if next == nil {
		t.Fatal("queued claim failed")
	}
	defer next.unused()
	if busy, ended, turns := slots.out("account"); busy != 1 || !ended.IsZero() || turns != 0 {
		t.Fatalf("unused claim fabricated completed turn: busy=%d ended=%v turns=%d", busy, ended, turns)
	}
	next.complete()
	next.unused()
	if busy, ended, turns := slots.out("account"); busy != 0 || ended.IsZero() || turns != 1 {
		t.Fatalf("actual completion must record once: busy=%d ended=%v turns=%d", busy, ended, turns)
	}
}

func TestLaneClaimFinalizerIsSharedOnce(t *testing.T) {
	for _, completeFirst := range []bool{false, true} {
		var slots lanes
		claim, ok := slots.tryClaim("account", 1)
		if !ok {
			t.Fatal("initial claim denied")
		}
		if completeFirst {
			claim.complete()
		} else {
			claim.unused()
		}
		var owners sync.WaitGroup
		for range 8 {
			owners.Go(func() { claim.complete(); claim.unused() })
		}
		owners.Wait()
		want := 0
		if completeFirst {
			want = 1
		}
		if busy, ended, turns := slots.out("account"); busy != 0 || turns != want || ended.IsZero() == completeFirst {
			t.Fatalf("shared once owner lost first disposition: firstComplete=%t busy=%d ended=%v turns=%d", completeFirst, busy, ended, turns)
		}
	}
}

func TestLaneClaimCanceledQueueHasNoCompletedReceipt(t *testing.T) {
	var slots lanes
	held, ok := slots.tryClaim("account", 1)
	if !ok {
		t.Fatal("initial claim denied")
	}
	defer held.unused()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	claim, _, err := slots.takeClaim(ctx, "account", 1, 0, 0, nil)
	if claim != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled queue returned a claim: claim=%v error=%v", claim, err)
	}
	if busy, ended, turns := slots.out("account"); busy != 1 || !ended.IsZero() || turns != 0 {
		t.Fatalf("canceled queue fabricated receipt: busy=%d ended=%v turns=%d", busy, ended, turns)
	}
}

func TestLaneClaimPolicyDenialDoesNotCompleteTurn(t *testing.T) {
	fresh(t)
	vendor := &scripted{replies: []reply{{200, "", chatOK}}}
	scriptedOn(t, "claim-policy", provider.Chat, vendor)
	ctx := ownPolicyContext(t, 0)
	root := requestpolicy.FromContext(ctx)
	before, _ := root.Counts("claim-policy")
	server := New()
	t.Cleanup(server.idle.stop)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claim-policy/m","messages":[{"role":"user","content":"no replay"}]}`)).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || vendor.n != 0 {
		t.Fatalf("denied root dispatched: status=%d vendor=%d body=%s", response.Code, vendor.n, response.Body)
	}
	p, err := provider.Find("claim-policy")
	if err != nil {
		t.Fatal(err)
	}
	who := candidateOf(*p, "m").who()
	if busy, ended, turns := server.lanes.out(who); busy != 0 || !ended.IsZero() || turns != 0 {
		t.Fatalf("policy rejection fabricated completed turn: busy=%d ended=%v turns=%d", busy, ended, turns)
	}
	if requestpolicy.FromContext(ctx) != root {
		t.Fatal("ordinary Front replaced the caller's root owner")
	}
	if used, _ := root.Counts("claim-policy"); used != before {
		t.Fatalf("policy rejection changed consumed root budget: before=%d after=%d", before, used)
	}
}

func TestLaneClaimRPMDenialDoesNotCompleteTurn(t *testing.T) {
	shortMinute(t, 3*time.Second, time.Millisecond)
	vendor := &minuteVendor{clock: holdMinute(t)}
	server, gateway := rpmGateway(t, vendor, 1)
	clock := vendor.clock
	if reply := clock.atOnce(t, "initial dispatch", askRPM(t.Context(), gateway, rlPath, rlBody)); reply.err != nil || reply.code != http.StatusOK {
		t.Fatalf("initial dispatch: %+v", reply)
	}
	who := meterWho(provider.Provider{ID: "rl", Key: "k"})
	_, originalEnd, originalTurns := server.lanes.out(who)
	reply := clock.atOnce(t, "RPM refusal", askRPM(t.Context(), gateway, rlPath, rlBody))
	if reply.err != nil || reply.code != http.StatusTooManyRequests || len(vendor.times()) != 1 {
		t.Fatalf("RPM refusal reached vendor: %+v calls=%d", reply, len(vendor.times()))
	}
	if busy, ended, turns := server.lanes.out(who); busy != 0 || !ended.Equal(originalEnd) || turns != originalTurns {
		t.Fatalf("RPM refusal fabricated completion: busy=%d ended=%v originalEnd=%v turns=%d originalTurns=%d",
			busy, ended, originalEnd, turns, originalTurns)
	}
}
