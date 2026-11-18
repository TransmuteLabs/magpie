package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/testenv"
)

type incomingHeldTurn struct {
	account string
	open    chan struct{}
	once    sync.Once
}

func (turn *incomingHeldTurn) release() {
	turn.once.Do(func() { close(turn.open) })
}

func incomingNearCapBehavior(t *testing.T, walkers int, requireProgress, denyPolicy bool) {
	t.Helper()
	codexSignedIn(t, "spare@example.com")
	ctx, cancel := context.WithTimeout(testenv.DeadlineContext(t, time.Second), 25*time.Second)
	defer cancel()
	walkerContext := ctx
	var mu sync.Mutex
	used := map[string]float64{"acct-1": 50, "acct-2": 20}
	var turns []*incomingHeldTurn
	expectedTurns := 3
	arrived := make(chan *incomingHeldTurn, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			mu.Lock()
			value := used[account]
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"plan_type": "plus",
				"rate_limit": map[string]any{"allowed": true,
					"primary_window": map[string]any{"used_percent": value, "limit_window_seconds": 18000,
						"reset_at": time.Now().Add(2 * time.Hour).Unix()},
					"secondary_window": map[string]any{"used_percent": 10, "limit_window_seconds": 604800,
						"reset_at": time.Now().Add(4 * 24 * time.Hour).Unix()}}})
		case "/backend-api/codex/responses":
			io.ReadAll(r.Body)
			turn := &incomingHeldTurn{account: account, open: make(chan struct{})}
			mu.Lock()
			turns = append(turns, turn)
			initial := len(turns) <= 3
			mu.Unlock()
			arrived <- turn
			if initial {
				select {
				case <-turn.open:
				case <-r.Context().Done():
					return
				}
			}
			io.WriteString(w, sse(
				`data: {"type":"response.created","response":{"id":"r1","model":"gpt-5.5"}}`,
				`data: {"type":"response.output_text.delta","delta":"pong"}`,
				`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":7,"output_tokens":1}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer up.Close()
	previousBase := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	defer func() { provider.CodexBase = previousBase }()
	nearCapAccounts(t, 90, 50, "spare@example.com")
	two, one := 2, 1
	if err := provider.SetAccountConcurrency("codex", "me@example.com", &two); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountConcurrency("codex", "spare@example.com", &one); err != nil {
		t.Fatal(err)
	}
	server := New()
	const whoB = "codex@me@example.com"
	const whoA = "codex@spare@example.com"
	replies := make(chan nearReply, 8)
	var owners sync.WaitGroup
	readerRelease := make(chan struct{})
	var releaseReader sync.Once
	unblock := func() { releaseReader.Do(func() { close(readerRelease) }) }
	defer func() {
		cancel()
		unblock()
		mu.Lock()
		for _, turn := range turns {
			turn.release()
		}
		mu.Unlock()
		joined := make(chan struct{})
		go func() { owners.Wait(); close(joined) }()
		select {
		case <-joined:
			t.Logf("all %d ordinary request owners joined; remaining lanes=%v nearWait=%d idleWait=%d",
				3+walkers, server.Lanes(), server.nearCap.waiting(), server.lanes.idling())
		case <-time.After(3 * time.Second):
			t.Error("ordinary request owners did not join after cancellation")
		}
		server.idle.stop()
		for who, lane := range server.Lanes() {
			if lane.Busy != 0 || lane.Waiting != 0 {
				t.Errorf("reservation retained after join: %s %+v", who, lane)
			}
		}
		server.nearCap.mu.Lock()
		for who, slot := range server.nearCap.slots {
			if len(slot) != 0 {
				t.Errorf("near-cap owner retained after join: %s", who)
			}
		}
		server.nearCap.mu.Unlock()
		usageContext, stopUsage := context.WithTimeout(testenv.DeadlineContext(t, time.Second), 3*time.Second)
		if !provider.ReadUsageNow(usageContext, "codex", "me@example.com", "spare@example.com") {
			t.Error("fixture usage owner did not settle before private HOME cleanup")
		}
		stopUsage()
		mu.Lock()
		sent := len(turns)
		mu.Unlock()
		if sent != expectedTurns {
			t.Errorf("vendor sends after join: got %d want %d; canceled or completed operations must not replay", sent, expectedTurns)
		}
	}()
	send := func(tag string) {
		owners.Go(func() {
			recorder := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"gpt-5.5","stream":true,"input":%q}`, tag)
			requestContext := ctx
			if strings.HasPrefix(tag, "walker-") {
				requestContext = walkerContext
			}
			request := httptest.NewRequest("POST", CodexPath+"/responses", strings.NewReader(body)).WithContext(requestContext)
			request.Header.Set("Authorization", "Bearer chatgpt-token")
			request.Header.Set("chatgpt-account-id", "acct-1")
			server.Handler().ServeHTTP(recorder, request)
			replies <- nearReply{recorder.Code, recorder.Body.String()}
		})
	}
	nextTurn := func() *incomingHeldTurn {
		t.Helper()
		select {
		case turn := <-arrived:
			return turn
		case <-ctx.Done():
			t.Fatal("setup did not reach the local vendor: ", ctx.Err())
			return nil
		}
	}
	var initial []*incomingHeldTurn
	for index := range 3 {
		send(fmt.Sprintf("initial-%d", index))
		initial = append(initial, nextTurn())
	}
	if initial[0].account != "acct-1" || initial[1].account != "acct-1" || initial[2].account != "acct-2" {
		t.Fatalf("initial ordinary selection: %s %s %s", initial[0].account, initial[1].account, initial[2].account)
	}
	mu.Lock()
	used["acct-1"] = 88
	mu.Unlock()
	if !provider.ReadUsageNow(ctx, "codex", "me@example.com") {
		t.Fatal("near-cap usage read did not start")
	}
	waitFor(t, "real 88 percent usage", func() bool {
		value, _ := provider.Allowances("codex")["me@example.com"].For("gpt-5.5", time.Now())
		return value == 88
	})
	if denyPolicy {
		zero := 0
		walkerContext = requestpolicy.WithConfig(ctx, &requestpolicy.Config{Defaults: requestpolicy.Options{MaxTotalRetries: &zero}})
		root := requestpolicy.FromContext(walkerContext)
		policy, err := root.Policy("outer", requestpolicy.Defaults())
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Dispatch(walkerContext, policy); err != nil {
			t.Fatal(err)
		}
	}
	for index := range walkers {
		send(fmt.Sprintf("walker-%d", index))
	}
	waitFor(t, "ordinary walks queued on full A after deferring B", func() bool {
		return server.Lanes()[whoA].Waiting == walkers
	})
	t.Logf("ordinary queue established: lanes=%v vendorTurns=3", server.Lanes())
	readerArrivals := make(chan struct{}, walkers)
	originalAllowances := allowances
	var readerMu sync.Mutex
	seen := 0
	allowances = func(agent string) map[string]provider.Allowance {
		result := originalAllowances(agent)
		lanes := server.Lanes()
		readerMu.Lock()
		hold := seen < walkers && lanes[whoB].Busy == 2 && lanes[whoA].Waiting < walkers
		if hold {
			seen++
		}
		readerMu.Unlock()
		if hold {
			readerArrivals <- struct{}{}
			select {
			case <-readerRelease:
			case <-ctx.Done():
			}
		}
		return result
	}
	t.Cleanup(func() { allowances = originalAllowances })
	initial[0].release()
	select {
	case <-readerArrivals:
	case <-ctx.Done():
		t.Fatal("first ordinary handoff did not reach the real allowance reader")
	}
	initial[1].release()
	if walkers == 2 {
		select {
		case <-readerArrivals:
		case <-ctx.Done():
			t.Fatal("second ordinary handoff did not reach the real allowance reader")
		}
	}
	t.Logf("actual handed-slot read barrier: walkers=%d lanes=%v", walkers, server.Lanes())
	if denyPolicy {
		for range 2 {
			select {
			case reply := <-replies:
				if reply.code != http.StatusOK || !strings.Contains(reply.body, "pong") {
					t.Fatalf("initial vendor completion: status=%d body=%s", reply.code, reply.body)
				}
			case <-ctx.Done():
				t.Fatal("initial vendor operations did not join before policy probe")
			}
		}
		_, originalEnd, originalTurns := server.lanes.out(whoB)
		unblock()
		waitFor(t, "actual handed policy refusal in ordinary routing trace", func() bool {
			for _, route := range server.Trace(ctx, 0, 0).Routes {
				for _, attempt := range route.Tries {
					if attempt.Status == http.StatusTooManyRequests && strings.Contains(attempt.Error, "Request retry budget exhausted") {
						return true
					}
				}
			}
			return false
		})
		if busy, ended, turns := server.lanes.out(whoB); busy != 0 || !ended.Equal(originalEnd) || turns != originalTurns {
			t.Fatalf("unused handed policy refusal fabricated completed turn: busy=%d ended=%v originalEnd=%v turns=%d originalTurns=%d",
				busy, ended, originalEnd, turns, originalTurns)
		}
		if used, selected := requestpolicy.FromContext(walkerContext).Counts("codex"); used != 0 || selected != 0 {
			t.Fatalf("denied handed operation replenished or charged root: used=%d selected=%d", used, selected)
		}
		t.Logf("actual handed policy refusal retained root and completion receipt: ended=%v turns=%d", originalEnd, originalTurns)
		return
	}
	if !requireProgress {
		cancel()
		unblock()
		t.Log("canceled both actual transferred operations before releasing their real allowance reader; join checks no late send")
		return
	}
	unblock()
	progressDeadline := time.NewTimer(2 * time.Second)
	defer progressDeadline.Stop()
	for index := range walkers {
		select {
		case turn := <-arrived:
			expectedTurns++
			if turn.account != "acct-1" {
				t.Fatalf("handed operation %d reached unexpected account %s", index, turn.account)
			}
		case <-progressDeadline.C:
			mu.Lock()
			sent := len(turns)
			mu.Unlock()
			t.Fatalf("ordinary handed operations failed vendor progress: got %d of %d; vendorTurns=%d lanes=%v idleWait=%d nearWait=%d",
				index, walkers, sent, server.Lanes(), server.lanes.idling(), server.nearCap.waiting())
		case <-ctx.Done():
			t.Fatal("enclosing deadline ended before ordinary operation progress")
		}
	}
	initial[2].release()
	for index := range 3 + walkers {
		select {
		case reply := <-replies:
			if reply.code != http.StatusOK || !strings.Contains(reply.body, "pong") {
				t.Fatalf("ordinary completion %d: status=%d body=%s", index, reply.code, reply.body)
			}
		case <-ctx.Done():
			t.Fatal("ordinary operations did not complete after vendor progress")
		}
	}
	t.Logf("all %d ordinary operations completed with successful pong replies; expected vendor turns=%d", 3+walkers, expectedTurns)
}

func TestIncomingNearCapHandoffBehaviorDouble(t *testing.T) {
	incomingNearCapBehavior(t, 2, true, false)
}

func TestIncomingNearCapHandoffBehaviorSingle(t *testing.T) {
	incomingNearCapBehavior(t, 1, true, false)
}

func TestIncomingNearCapHandoffBehaviorCancellation(t *testing.T) {
	incomingNearCapBehavior(t, 2, false, false)
}

func TestIncomingNearCapHandoffPolicyRefusalNoCompletedReceipt(t *testing.T) {
	incomingNearCapBehavior(t, 1, true, true)
}
