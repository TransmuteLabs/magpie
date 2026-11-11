package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

const resetFixtureScope = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const resetFixtureContext = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type zcodeResetVendor struct {
	mu            sync.Mutex
	now           time.Time
	context       string
	five          int
	week          int
	fiveUsed      float64
	weekUsed      float64
	uses          []string
	kinds         []string
	used          map[string]bool
	oppKeys       []string
	oppCodes      []float64
	loseFirst     bool
	hiddenHistory float64
}

func newZCodeResetVendor(t *testing.T) (*zcodeResetVendor, zcodeResetAccount) {
	t.Helper()
	detectHome(t)
	v := &zcodeResetVendor{now: time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC), context: resetFixtureContext, five: 1, week: 1, used: map[string]bool{}}
	oldNow := zcodeResetNow
	zcodeResetNow = func() time.Time { v.mu.Lock(); defer v.mu.Unlock(); return v.now }
	t.Cleanup(func() { zcodeResetNow = oldNow })
	return v, zcodeResetAccount{provider: "zcode", key: "zcode#fixture", user: "person@example.com", via: v.fetch}
}

func resetFixtureResponse(context string, code float64, data any, status int) *http.Response {
	b, err := json.Marshal(struct {
		Code float64 `json:"code"`
		Data any     `json:"data"`
	}{code, data})
	if err != nil {
		panic(err)
	}
	h := http.Header{}
	h.Set(zcodeResetScopeHeader, resetFixtureScope)
	h.Set(zcodeResetContextHeader, context)
	h.Set("X-Magpie-ZCode-Reset-Target", "PERSONAL")
	h.Set("X-Magpie-ZCode-Reset-Dispatched", "1")
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(b))}
}

func (v *zcodeResetVendor) fetch(_ context.Context, req plugin.FetchRequest) (*http.Response, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if expected := req.Headers["x-magpie-zcode-reset-context"]; expected != "" && expected != v.context {
		res := resetFixtureResponse(v.context, 0, nil, http.StatusConflict)
		res.Header.Set("X-Magpie-ZCode-Reset-Error", "account_changed")
		res.Header.Set("X-Magpie-ZCode-Reset-Dispatched", "0")
		return res, nil
	}
	operation := strings.TrimPrefix(req.URL, zcodeResetURL)
	if operation == "status" {
		pool := func(n int) []map[string]float64 {
			out := make([]map[string]float64, n)
			for i := range out {
				out[i] = map[string]float64{"expire_at": float64(v.now.Add(time.Hour).UnixMilli())}
			}
			return out
		}
		history := func(at float64) any {
			if at == 0 {
				return nil
			}
			return map[string]float64{"used_at": at}
		}
		return resetFixtureResponse(v.context, 0, map[string]any{
			"available_five_hour_resets": pool(v.five), "available_week_resets": pool(v.week),
			"latest_five_hour_reset_history": history(v.fiveUsed), "latest_week_reset_history": history(v.weekUsed), "has_unread_history": false,
		}, http.StatusOK), nil
	}
	if operation == "history/read" {
		if len(req.Body) != 0 {
			return nil, errors.New("history/read has no body")
		}
		return resetFixtureResponse(v.context, 0, nil, http.StatusOK), nil
	}
	var body struct {
		Key  string `json:"idempotency_key"`
		Kind string `json:"reset_type"`
	}
	if json.Unmarshal(req.Body, &body) != nil || body.Key == "" {
		return nil, errors.New("invalid mutation request")
	}
	if operation == "opportunity" {
		v.oppKeys = append(v.oppKeys, body.Key)
		code := float64(0)
		if len(v.oppCodes) > 0 {
			code, v.oppCodes = v.oppCodes[0], v.oppCodes[1:]
		}
		if code == 3301 {
			return resetFixtureResponse(v.context, code, map[string]any{"granted": false, "next_try_at": v.now.Add(2 * time.Minute).UnixMilli()}, http.StatusTooManyRequests), nil
		}
		if code != 0 {
			return resetFixtureResponse(v.context, code, nil, http.StatusOK), nil
		}
		return resetFixtureResponse(v.context, 0, map[string]bool{"granted": true}, http.StatusOK), nil
	}
	if operation != "use" {
		return nil, errors.New("unexpected vendor operation")
	}
	v.uses, v.kinds = append(v.uses, body.Key), append(v.kinds, body.Kind)
	if !v.used[body.Key] {
		v.used[body.Key] = true
		v.now = v.now.Add(time.Millisecond)
		if body.Kind == "FIVE_HOUR" {
			v.five--
			v.fiveUsed = float64(v.now.UnixMilli())
		} else {
			v.week--
			v.weekUsed = float64(v.now.UnixMilli())
		}
		if v.loseFirst {
			v.loseFirst = false
			v.hiddenHistory, v.fiveUsed = v.fiveUsed, 0
			return nil, errors.New("the reply was lost after the vendor debit")
		}
	} else if v.hiddenHistory != 0 {
		v.fiveUsed, v.hiddenHistory = v.hiddenHistory, 0
	}
	return resetFixtureResponse(v.context, 0, map[string]bool{"used": true}, http.StatusOK), nil
}

func TestZCodeResetUnknownUseKeepsItsKeyAcrossCalls(t *testing.T) {
	v, account := newZCodeResetVendor(t)
	v.loseFirst = true
	out, err := spendZCodeReset(context.Background(), account, "fiveHour", resetFixtureContext)
	if err != nil || out.Code != "reset_unconfirmed" {
		t.Fatalf("unknown debit: %+v %v", out, err)
	}
	var pending string
	if err := withZCodeResetState(context.Background(), resetFixtureScope, func(r *zcodeResetRecord) (bool, error) { pending = r.Pending.Key; return false, nil }); err != nil {
		t.Fatal(err)
	}
	// A fresh account client has none of the first caller's in-memory state.
	restarted := zcodeResetAccount{provider: account.provider, key: account.key, user: account.user, via: v.fetch}
	out, err = spendZCodeReset(context.Background(), restarted, "fiveHour", resetFixtureContext)
	if err != nil || out.Code != "reset" {
		t.Fatalf("reconciled debit: %+v %v", out, err)
	}
	if len(v.used) != 1 || v.five != 0 || v.week != 1 || len(v.uses) != 2 || v.uses[0] != pending || v.uses[1] != pending {
		t.Fatalf("extra/wrong debit: %+v", v)
	}
}

func TestZCodeResetPendingIsAdoptedOnlyForItsWindow(t *testing.T) {
	v, account := newZCodeResetVendor(t)
	now := v.now
	err := withZCodeResetState(context.Background(), resetFixtureScope, func(r *zcodeResetRecord) (bool, error) {
		r.Seen = true
		r.Pending = &zcodePendingReset{Window: "weekly", Key: "weekly-unknown", Baseline: 100, Started: now, Claim: "another-caller", ClaimedAt: now}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	v.fiveUsed = 200
	out, err := spendZCodeReset(context.Background(), account, "weekly", resetFixtureContext)
	if err != nil || out.Code != "reset_unconfirmed" || len(v.uses) != 0 {
		t.Fatalf("five hours adopted as week: %+v %v uses=%v", out, err, v.uses)
	}
	v.weekUsed = 300
	out, err = spendZCodeReset(context.Background(), account, "weekly", resetFixtureContext)
	if err != nil || out.Code != "already_used" || len(v.uses) != 0 {
		t.Fatalf("week not adopted: %+v %v uses=%v", out, err, v.uses)
	}
}

func TestZCodeResetOneDebitWhileAnotherCallerIsInFlight(t *testing.T) {
	v, account := newZCodeResetVendor(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	original := account.via
	account.via = func(ctx context.Context, req plugin.FetchRequest) (*http.Response, error) {
		if strings.HasSuffix(req.URL, "/use") {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return original(ctx, req)
	}
	type result struct {
		out ResetOutcome
		err error
	}
	first := make(chan result, 1)
	joined := false
	defer func() {
		unblock()
		cancel()
		if !joined {
			<-first
		}
	}()
	go func() {
		out, err := spendZCodeReset(ctx, account, "fiveHour", resetFixtureContext)
		first <- result{out, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first caller never reached use")
	}
	second, err := spendZCodeReset(ctx, account, "fiveHour", resetFixtureContext)
	unblock()
	one := <-first
	joined = true
	if err != nil || second.Code != "reset_unconfirmed" || one.err != nil || one.out.Code != "reset" {
		t.Fatalf("callers: %+v %v / %+v", second, err, one)
	}
	if len(v.used) != 1 || v.five != 0 || v.week != 1 {
		t.Fatalf("wrong debit: used=%v five=%d week=%d", v.used, v.five, v.week)
	}
}

func TestZCodeResetPeerCompletionCoversOnlyItsWindow(t *testing.T) {
	for _, window := range []string{"fiveHour", "weekly"} {
		t.Run(window, func(t *testing.T) {
			v, account := newZCodeResetVendor(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			second := account
			initial := true
			second.via = func(ctx context.Context, req plugin.FetchRequest) (*http.Response, error) {
				if initial && strings.HasSuffix(req.URL, "/status") {
					initial = false
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return v.fetch(ctx, req)
			}
			type result struct {
				out ResetOutcome
				err error
			}
			done := make(chan result, 1)
			joined := false
			defer func() {
				unblock()
				cancel()
				if !joined {
					<-done
				}
			}()
			go func() {
				out, err := spendZCodeReset(ctx, second, window, resetFixtureContext)
				done <- result{out, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("second caller never requested status")
			}
			first, firstErr := spendZCodeReset(ctx, account, "fiveHour", resetFixtureContext)
			unblock()
			got := <-done
			joined = true
			want, debits := "already_used", 1
			if window == "weekly" {
				want, debits = "reset", 2
			}
			if firstErr != nil || first.Code != "reset" || got.err != nil || got.out.Code != want || len(v.used) != debits {
				t.Fatalf("covered windows: first=%+v/%v second=%+v/%v debits=%v", first, firstErr, got.out, got.err, v.used)
			}
		})
	}
}

func TestZCodeResetOpportunityCadenceAndKeyOwnership(t *testing.T) {
	v, account := newZCodeResetVendor(t)
	v.oppCodes = []float64{2007, 3301, 0}
	read := func() {
		t.Helper()
		resets, why := fetchZCodeResets(context.Background(), account, resetFixtureScope, resetFixtureContext)
		if why != "" || resets.FiveHour != 1 {
			t.Fatalf("quota/reset inventory lost: %+v %s", resets, why)
		}
	}
	read() // Holding a credit does not suppress an opportunity.
	v.now = v.now.Add(4 * time.Minute)
	read()
	if len(v.oppKeys) != 1 {
		t.Fatalf("transient cooldown skipped: %v", v.oppKeys)
	}
	v.now = v.now.Add(time.Minute)
	read()
	if len(v.oppKeys) != 2 || v.oppKeys[0] != v.oppKeys[1] {
		t.Fatalf("transient key changed: %v", v.oppKeys)
	}
	v.now = v.now.Add(4 * time.Minute)
	read()
	if len(v.oppKeys) != 2 {
		t.Fatalf("3301 minimum cooldown skipped: %v", v.oppKeys)
	}
	v.now = v.now.Add(time.Minute)
	read()
	if len(v.oppKeys) != 3 || v.oppKeys[2] == v.oppKeys[1] {
		t.Fatalf("denial key reused after completion: %v", v.oppKeys)
	}
	if len(v.uses) != 0 {
		t.Fatalf("opportunity spent a reset: %v", v.uses)
	}
}

func TestZCodeResetOpportunityFailureCadence(t *testing.T) {
	for _, mode := range []string{"business-3101", "http-429", "http-500", "body-interrupted"} {
		t.Run(mode, func(t *testing.T) {
			v, account := newZCodeResetVendor(t)
			if mode == "business-3101" {
				v.oppCodes = []float64{3101, 0}
			}
			original := account.via
			account.via = func(ctx context.Context, req plugin.FetchRequest) (*http.Response, error) {
				res, err := original(ctx, req)
				if err == nil && strings.HasSuffix(req.URL, "/opportunity") && len(v.oppKeys) == 1 {
					switch mode {
					case "http-429":
						res.StatusCode = http.StatusTooManyRequests
					case "http-500":
						res.StatusCode = http.StatusInternalServerError
					case "body-interrupted":
						res.Body.Close()
						r, w := io.Pipe()
						w.CloseWithError(io.ErrUnexpectedEOF)
						res.Body = r
					}
				}
				return res, err
			}
			read := func() {
				t.Helper()
				resets, why := fetchZCodeResets(context.Background(), account, resetFixtureScope, resetFixtureContext)
				if why != "" || resets.FiveHour != 1 {
					t.Fatalf("opportunity failure erased the quota: %+v %s", resets, why)
				}
			}
			read()
			v.now = v.now.Add(5 * time.Minute)
			read()
			if mode == "body-interrupted" {
				if len(v.oppKeys) != 2 || v.oppKeys[0] != v.oppKeys[1] {
					t.Fatalf("interrupted reply lost the retry key: %v", v.oppKeys)
				}
				return
			}
			if len(v.oppKeys) != 1 {
				t.Fatalf("stable refusal retried before ten minutes: %v", v.oppKeys)
			}
			v.now = v.now.Add(5 * time.Minute)
			read()
			if len(v.oppKeys) != 2 || v.oppKeys[0] == v.oppKeys[1] {
				t.Fatalf("stable refusal reused its completed key: %v", v.oppKeys)
			}
		})
	}
}

func TestZCodeResetStateFailureNeverStartsUse(t *testing.T) {
	for _, mode := range []string{"corrupt", "locked"} {
		t.Run(mode, func(t *testing.T) {
			v, account := newZCodeResetVendor(t)
			if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				if err := os.WriteFile(zcodeResetStatePath(), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				unlock, held := filelock.Lock(zcodeResetStatePath()+".lock", 0)
				if !held {
					t.Fatal("fixture did not hold the lock")
				}
				defer unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, err := spendZCodeReset(ctx, account, "fiveHour", resetFixtureContext); err == nil {
				t.Fatal("unowned state allowed a financial operation")
			}
			if len(v.uses) != 0 {
				t.Fatalf("sent a use without state ownership: %v", v.uses)
			}
			if mode == "corrupt" {
				b, _ := os.ReadFile(zcodeResetStatePath())
				if string(b) != "{" {
					t.Fatal("corrupt state was reinitialized")
				}
			}
		})
	}
}

func TestZCodeResetOldSnapshotDoesNotSpendNewSelection(t *testing.T) {
	v, account := newZCodeResetVendor(t)
	v.context = strings.Repeat("c", 64)
	if _, err := spendZCodeReset(context.Background(), account, "fiveHour", resetFixtureContext); err == nil {
		t.Fatal("old context accepted")
	}
	if len(v.uses) != 0 {
		t.Fatalf("old context spent: %v", v.uses)
	}
	if _, err := os.Stat(filepath.Join(settings.Dir(), "zcode-resets.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old context touched the financial state: %v", err)
	}
}

func TestZCodeResetEnvelopeBusinessPrecedesHTTP(t *testing.T) {
	_, account := newZCodeResetVendor(t)
	account.via = func(context.Context, plugin.FetchRequest) (*http.Response, error) {
		return resetFixtureResponse(resetFixtureContext, 3301, map[string]any{"granted": false, "next_try_at": 2_000_000_000_000}, http.StatusTooManyRequests), nil
	}
	reply, err := account.call(context.Background(), "opportunity", resetFixtureContext, map[string]string{"idempotency_key": "key"})
	if err != nil || reply.code != 3301 {
		t.Fatalf("business denial became bare HTTP: %+v %v", reply, err)
	}
	account.via = func(context.Context, plugin.FetchRequest) (*http.Response, error) {
		return resetFixtureResponse(resetFixtureContext, 0, map[string]bool{"used": true}, http.StatusInternalServerError), nil
	}
	if _, err := account.call(context.Background(), "use", resetFixtureContext, map[string]string{"idempotency_key": "key"}); err == nil {
		t.Fatal("code 0 accepted over failed HTTP")
	}
	if _, err := decodeZCodeResetStatus(json.RawMessage(`{"available_five_hour_resets":[],"available_week_resets":[],"has_unread_history":false}`)); err == nil {
		t.Fatal("missing history treated as empty")
	}
}
