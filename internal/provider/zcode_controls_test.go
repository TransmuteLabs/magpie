package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func zcodeTestContext(t *testing.T) plugin.AccountContext {
	t.Helper()
	isolateRefreshes(t)
	return plugin.AccountContext{Scope: t.Name(), Generation: "login-a", Origin: "https://zcode.z.ai", Family: "zai", UserID: "user-a", Kind: "individual"}
}

func zcodeTestRPC(t *testing.T, fn func(plugin.AccountRequest) (plugin.AccountResult, error)) {
	t.Helper()
	old := zcodeAccountRPC
	zcodeAccountRPC = func(_ context.Context, _, _ string, request plugin.AccountRequest) (plugin.AccountResult, error) {
		return fn(request)
	}
	t.Cleanup(func() { zcodeAccountRPC = old })
}

func zcodeTestData(t *testing.T, c plugin.AccountContext, value any) plugin.AccountResult {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return plugin.AccountResult{Context: c, Data: b}
}

func resetFixture(expiry int64, latest int64, unread bool) map[string]any {
	var history any
	if latest > 0 {
		history = map[string]any{"used_at": latest}
	}
	return map[string]any{
		"available_five_hour_resets":     []any{map[string]any{"expire_at": expiry}},
		"available_week_resets":          []any{map[string]any{"expire_at": expiry}},
		"latest_five_hour_reset_history": history,
		"latest_week_reset_history":      nil,
		"has_unread_history":             unread,
	}
}

func TestZCodePublicResetRejectsUnboundAndLegacyWindows(t *testing.T) {
	calls := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		calls++
		return plugin.AccountResult{}, errors.New("unexpected dispatch")
	})
	if _, err := UseZCodeReset(context.Background(), "zcode", "zcode", "week"); err == nil {
		t.Fatal("unbound reset was accepted")
	}
	ctx := WithZCodeAccountExpectation(context.Background(), "scope", "generation")
	for _, window := range []string{"fiveHour", "weekly", "WEEK", " five-hour", "month"} {
		if _, err := UseZCodeReset(ctx, "zcode", "zcode", window); err == nil {
			t.Errorf("legacy or noncanonical window %q was accepted", window)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid reset dispatched %d calls", calls)
	}
}

// An existing reset does not suppress opportunity; a grant forces another
// authoritative status read before the caller sees the card count.
func TestZCodeOpportunityWithExistingCards(t *testing.T) {
	c := zcodeTestContext(t)
	expiry := time.Now().Add(time.Hour).UnixMilli()
	var calls []string
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		calls = append(calls, req.Operation)
		switch req.Operation {
		case "reset.status":
			v := resetFixture(expiry, 0, false)
			if len(calls) > 1 {
				v["available_five_hour_resets"] = []any{map[string]any{"expire_at": expiry}, map[string]any{"expire_at": expiry + 1000}}
			}
			return zcodeTestData(t, c, v), nil
		case "reset.opportunity":
			p := req.Params.(map[string]any)
			if p["idempotency_key"] == "" {
				t.Error("opportunity lacked durable idempotency key")
			}
			return zcodeTestData(t, c, map[string]any{"granted": true}), nil
		default:
			t.Fatalf("unexpected operation %s", req.Operation)
			return plugin.AccountResult{}, nil
		}
	})
	state, err := zcodeResetStatus(context.Background(), "zcode", "zcode#1", c, true)
	if err != nil || len(state.FiveHour) != 2 || strings.Join(calls, ",") != "reset.status,reset.opportunity,reset.status" {
		t.Fatalf("state=%+v calls=%v err=%v", state, calls, err)
	}
}

func TestZCodeOpportunityRetryKeepsItsKey(t *testing.T) {
	c := zcodeTestContext(t)
	var keys []string
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		keys = append(keys, req.Params.(map[string]any)["idempotency_key"].(string))
		if len(keys) == 1 {
			return plugin.AccountResult{}, &plugin.AccountError{Message: "timeout", Retryable: true, Dispatched: true}
		}
		return zcodeTestData(t, c, map[string]any{"granted": true}), nil
	})
	if _, err := zcodeOpportunity(context.Background(), "zcode", "zcode#1", c); err == nil {
		t.Fatal("timed-out opportunity reported a grant")
	}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		j.Reset.OpportunityAt = 0
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if granted, err := zcodeOpportunity(context.Background(), "zcode", "zcode#1", c); err != nil || !granted {
		t.Fatalf("retry: granted=%v err=%v", granted, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("opportunity retry keys=%q", keys)
	}
}

// A lost reply remains pending across login epochs. A newer timestamp is an
// observed completion, not evidence that the lost POST caused it.
func TestZCodeResetLostReplySurvivesRelogin(t *testing.T) {
	c := zcodeTestContext(t)
	expiry := time.Now().Add(time.Hour).UnixMilli()
	latest, uses := int64(100), 0
	var historyPersisted bool
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "reset.status":
			return zcodeTestData(t, c, resetFixture(expiry, latest, latest > 100)), nil
		case "reset.use":
			uses++
			return plugin.AccountResult{}, &plugin.AccountError{Message: "connection lost", Dispatched: true, Retryable: true}
		case "reset.history.read":
			if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
				historyPersisted = j.Reset.FiveHigh == float64(latest) && j.Reset.Pending != nil && j.Reset.Pending.ObservedAt == float64(latest)
				return false, nil
			}); err != nil {
				t.Error(err)
			}
			return zcodeTestData(t, c, nil), nil
		default:
			t.Fatalf("unexpected operation %s", req.Operation)
			return plugin.AccountResult{}, nil
		}
	})
	if _, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "five-hour"); err == nil {
		t.Fatal("lost use reply reported success")
	}
	c.Generation = "login-b"
	latest = 101
	_, state, _, err := zcodeResetRead(context.Background(), "zcode", "zcode#1", c)
	if err != nil || state.Pending != "observed-completion" || state.ObservedAt != 101 || !historyPersisted {
		t.Fatalf("state=%+v persisted before acknowledgement=%v err=%v", state, historyPersisted, err)
	}
	if _, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "five-hour"); err == nil || uses != 1 {
		t.Fatalf("unresolved reset admitted a second use: calls=%d err=%v", uses, err)
	}
	if err := resolveZCodeReset(c); err != nil {
		t.Fatal(err)
	}
}

func TestZCodeResetUnavailableStillReportsDurablePending(t *testing.T) {
	c := zcodeTestContext(t)
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		j.Reset.WeekHigh = 100
		j.Reset.Pending = &zcodeResetClaim{Key: "claim-a", Window: "week", Generation: c.Generation, Baseline: 100, HighWater: 100}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		return plugin.AccountResult{}, errors.New("status unavailable")
	})
	state, err := zcodeResetStatus(context.Background(), "zcode", "zcode#1", c, false)
	if err == nil || state.Source != "zcode" || state.Available || state.Pending != "unconfirmed" || state.PendingWindow != "week" {
		t.Fatalf("unavailable status lost the pending claim: state=%+v err=%v", state, err)
	}
}

func TestZCodeResetAcknowledgesPreviouslyPersistedUnreadHistory(t *testing.T) {
	c := zcodeTestContext(t)
	unread, acks := false, 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "reset.status":
			return zcodeTestData(t, c, resetFixture(time.Now().Add(time.Hour).UnixMilli(), 100, unread)), nil
		case "reset.history.read":
			acks++
			return zcodeTestData(t, c, nil), nil
		default:
			return plugin.AccountResult{}, errors.New("unexpected operation")
		}
	})
	if _, _, _, err := zcodeResetRead(context.Background(), "zcode", "zcode#1", c); err != nil {
		t.Fatal(err)
	}
	unread = true
	if _, _, _, err := zcodeResetRead(context.Background(), "zcode", "zcode#1", c); err != nil || acks != 1 {
		t.Fatalf("previously persisted unread history: acks=%d err=%v", acks, err)
	}
}

// Out-of-order old history cannot confirm an accepted use; once a genuinely
// newer history arrives it is persisted and the pending claim is cleared.
func TestZCodeResetAcceptedWaitsForNewHistory(t *testing.T) {
	c := zcodeTestContext(t)
	expiry := time.Now().Add(time.Hour).UnixMilli()
	latest, reads := int64(100), 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "reset.status":
			reads++
			if reads == 2 {
				latest = 99
			}
			if reads == 3 {
				latest = 101
			}
			return zcodeTestData(t, c, resetFixture(expiry, latest, false)), nil
		case "reset.use":
			return zcodeTestData(t, c, map[string]bool{"used": true}), nil
		default:
			return plugin.AccountResult{}, fmt.Errorf("unexpected operation %s", req.Operation)
		}
	})
	state, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "five-hour")
	if err != nil || state.Pending != "" || state.Last == nil || state.Last.UsedAt != 101 || reads != 3 {
		t.Fatalf("state=%+v reads=%d err=%v", state, reads, err)
	}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		if j.Reset.FiveHigh != 101 || j.Reset.Pending != nil {
			t.Errorf("journal=%+v", j.Reset)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestZCodeResetAcceptsFractionalHistoryMilliseconds(t *testing.T) {
	c := zcodeTestContext(t)
	expiry := time.Now().Add(time.Hour).UnixMilli()
	reads := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "reset.status":
			reads++
			v := resetFixture(expiry, 0, false)
			used := 100.25
			if reads > 1 {
				used = 100.5
			}
			v["latest_five_hour_reset_history"] = map[string]any{"used_at": used}
			return zcodeTestData(t, c, v), nil
		case "reset.use":
			return zcodeTestData(t, c, map[string]bool{"used": true}), nil
		default:
			return plugin.AccountResult{}, errors.New("unexpected operation")
		}
	})
	state, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "five-hour")
	if err != nil || state.Pending != "" || state.Last == nil || state.Last.UsedAt != 100.5 {
		t.Fatalf("fractional history: state=%+v err=%v", state, err)
	}
}

func TestZCodeCorruptJournalFailsBeforeSpending(t *testing.T) {
	c := zcodeTestContext(t)
	path := zcodeJournalPath(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		calls++
		return zcodeTestData(t, c, resetFixture(time.Now().Add(time.Hour).UnixMilli(), 0, false)), nil
	})
	for _, broken := range []string{`{"version":1,"scope":`, `{}`, `{"version":1,"scope":"` + c.Scope + `","origin":"` + c.Origin + `"}`} {
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "week"); err == nil || calls != 0 {
			t.Fatalf("corrupt journal %q: err=%v vendor calls=%d", broken, err, calls)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "week"); err == nil || calls != 0 {
		t.Fatalf("unreadable journal: err=%v vendor calls=%d", err, calls)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE unrelated (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "week"); err == nil || calls != 0 {
		t.Fatalf("existing database without journal schema: err=%v vendor calls=%d", err, calls)
	}
}

func TestZCodeResetDoesNotStealLiveClaim(t *testing.T) {
	c := zcodeTestContext(t)
	expiry := time.Now().Add(time.Hour).UnixMilli()
	arrived, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	finished := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); <-finished })
	uses := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "reset.status":
			return zcodeTestData(t, c, resetFixture(expiry, 0, false)), nil
		case "reset.use":
			uses++
			close(arrived)
			<-release
			return plugin.AccountResult{}, errors.New("reply lost")
		default:
			return plugin.AccountResult{}, errors.New("unexpected operation")
		}
	})
	go func() {
		defer close(finished)
		_, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "week")
		done <- err
	}()
	waitRefresh(t, arrived)
	if err := resolveZCodeReset(c); err == nil {
		t.Fatal("operator resolution stole a live use")
	}
	if _, err := useZCodeReset(context.Background(), "zcode", "zcode#1", c, "week"); err == nil {
		t.Fatal("second use stole a live use")
	}
	once.Do(func() { close(release) })
	if err := <-done; err == nil || uses != 1 {
		t.Fatalf("lost reply=%v, calls=%d", err, uses)
	}
}

func TestZCodeExecutionBindingAndCooldown(t *testing.T) {
	c := zcodeTestContext(t)
	now := time.Now().UnixMilli()
	var mu sync.Mutex
	calls := map[string]int{}
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		mu.Lock()
		calls[req.Operation]++
		mu.Unlock()
		switch req.Operation {
		case "context":
			return zcodeTestData(t, c, nil), nil
		case "highspeed.draw":
			return zcodeTestData(t, c, map[string]any{"card": map[string]any{"card_id": "card-a", "task_id": "session-a", "model": "GLM-5.3", "expires_at": now + 60000}, "next_draw_at": now + 60000}), nil
		default:
			return plugin.AccountResult{}, fmt.Errorf("unexpected operation %s", req.Operation)
		}
	})
	p := zcodeExecutionParams{TaskID: "session-a", Model: "GLM-5.3"}
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, p, "highspeed", false); err != nil {
		t.Fatal(err)
	}
	raw, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", p.TaskID, "glm-5.3-highspeed")
	if err != nil {
		t.Fatal(err)
	}
	var binding map[string]any
	if err := json.Unmarshal(raw, &binding); err != nil {
		t.Fatal(err)
	}
	if binding["taskId"] != p.TaskID || binding["rawModel"] != "GLM-5.3" || binding["cardId"] != "card-a" || binding["expiresAt"] != float64(now+60000) {
		t.Fatalf("binding=%v", binding)
	}
	if _, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", "another", "GLM-5.3-Highspeed"); err == nil {
		t.Fatal("another task used the card")
	}
	if _, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", p.TaskID, "GLM-5.3-Flash-Highspeed"); err == nil {
		t.Fatal("another model used the card")
	}
	c.Generation = "login-b"
	if _, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", p.TaskID, "GLM-5.3-Highspeed"); err == nil {
		t.Fatal("another login used the card")
	}
	c.Generation = "login-a"
	if _, err := zcodeExecutionFinish(context.Background(), "zcode", "zcode#1", c, p); err != nil {
		t.Fatal(err)
	}
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, zcodeExecutionParams{TaskID: "session-b", Model: "GLM-5.3"}, "highspeed", false); err == nil {
		t.Fatal("cooldown was lost on finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["highspeed.draw"] != 1 || calls["offpeak.settle"] != 0 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestZCodeExecutionUnknownAdmissionNeverRetakes(t *testing.T) {
	c := zcodeTestContext(t)
	calls := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		if req.Operation == "offpeak.ticket" {
			calls++
			return plugin.AccountResult{}, errors.New("connection lost")
		}
		return plugin.AccountResult{}, errors.New("unexpected operation")
	})
	p := zcodeExecutionParams{TaskID: "task-a", Model: "GLM-5.3"}
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, p, "offpeak", false); err == nil {
		t.Fatal("unknown admission reported success")
	}
	c.Generation = "login-b"
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, p, "offpeak", false); err == nil || calls != 1 {
		t.Fatalf("re-login retook a possibly admitted ticket: calls=%d err=%v", calls, err)
	}
}

func TestOffPeakRequiresReadyAndSettlesExplicitly(t *testing.T) {
	c := zcodeTestContext(t)
	settled := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "context":
			return zcodeTestData(t, c, nil), nil
		case "offpeak.ticket":
			return zcodeTestData(t, c, map[string]any{"ticket_id": "ticket-a", "task_id": "task-a", "state": "queued"}), nil
		case "offpeak.status":
			return zcodeTestData(t, c, map[string]any{"tickets": []any{map[string]any{"ticket_id": "ticket-a", "task_id": "task-a", "state": "ready"}}}), nil
		case "offpeak.settle":
			settled++
			return zcodeTestData(t, c, map[string]any{}), nil
		default:
			return plugin.AccountResult{}, fmt.Errorf("unexpected operation %s", req.Operation)
		}
	})
	p := zcodeExecutionParams{TaskID: "task-a", Model: "GLM-5.3"}
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, p, "offpeak", false); err != nil {
		t.Fatal(err)
	}
	if _, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", p.TaskID, "GLM-5.3-OffPeak"); err == nil {
		t.Fatal("a queued ticket was dispatched")
	}
	if _, err := zcodeExecutionStatus(context.Background(), "zcode", "zcode#1", c, p); err != nil {
		t.Fatal(err)
	}
	if _, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", p.TaskID, "GLM-5.3-OffPeak"); err != nil {
		t.Fatal(err)
	}
	if settled != 0 {
		t.Fatal("status or inference settled the task")
	}
	if _, err := zcodeExecutionFinish(context.Background(), "zcode", "zcode#1", c, p); err != nil || settled != 1 {
		t.Fatalf("explicit finish: settled=%d err=%v", settled, err)
	}
}

func TestOffPeakLostSettlementReconcilesWithoutResending(t *testing.T) {
	c := zcodeTestContext(t)
	settles := 0
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "offpeak.ticket":
			return zcodeTestData(t, c, map[string]any{"ticket_id": "ticket-a", "task_id": "task-a", "state": "ready"}), nil
		case "offpeak.settle":
			settles++
			return plugin.AccountResult{}, errors.New("reply lost")
		case "offpeak.status":
			return zcodeTestData(t, c, map[string]any{"tickets": []any{map[string]any{"ticket_id": "ticket-a", "task_id": "task-a", "state": "settled"}}}), nil
		default:
			return plugin.AccountResult{}, errors.New("unexpected operation")
		}
	})
	p := zcodeExecutionParams{TaskID: "task-a", Model: "GLM-5.3"}
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, p, "offpeak", false); err != nil {
		t.Fatal(err)
	}
	if _, err := zcodeExecutionFinish(context.Background(), "zcode", "zcode#1", c, p); err == nil {
		t.Fatal("lost settlement reply reported success")
	}
	if _, err := zcodeExecutionStatus(context.Background(), "zcode", "zcode#1", c, p); err != nil {
		t.Fatal(err)
	}
	if _, err := zcodeExecutionFinish(context.Background(), "zcode", "zcode#1", c, p); err == nil || settles != 1 {
		t.Fatalf("settlement was replayed: calls=%d err=%v", settles, err)
	}
}

func TestOffPeakFractionalExpiredReadyDeadlineCannotDispatch(t *testing.T) {
	c := zcodeTestContext(t)
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "context":
			return zcodeTestData(t, c, nil), nil
		case "offpeak.ticket":
			return zcodeTestData(t, c, map[string]any{"ticket_id": "ticket-a", "task_id": "task-a", "state": "ready", "ready_deadline": float64(time.Now().UnixMilli()-1000) + .5}), nil
		default:
			return plugin.AccountResult{}, errors.New("unexpected operation")
		}
	})
	p := zcodeExecutionParams{TaskID: "task-a", Model: "GLM-5.3"}
	if _, err := zcodeExecutionStart(context.Background(), "zcode", "zcode#1", c, p, "offpeak", false); err != nil {
		t.Fatal(err)
	}
	if _, err := zcodeExecutionFor(context.Background(), "zcode", "zcode#1", p.TaskID, "GLM-5.3-OffPeak"); err == nil {
		t.Fatal("expired ready ticket was dispatched")
	}
}
