package provider

// ZCode resets use the existing plugin fetch and reset controls. The broker
// keeps only operation keys and server observations, never account credentials.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
)

const (
	zcodeResetURL           = "https://zcode.z.ai/api/v1/coding-plan/reset/"
	zcodeResetScopeHeader   = "X-Magpie-ZCode-Reset-Scope"
	zcodeResetContextHeader = "X-Magpie-ZCode-Reset-Context"
	zcodeResetClaimWait     = 30 * time.Second
)

var zcodeResetNow = time.Now

// via is the same authenticated-fetch boundary the existing plugin check-ins
// use. Tests supply a vendor fixture at that boundary, not account secrets.
type zcodeResetAccount struct {
	provider string
	key      string
	user     string
	via      func(context.Context, plugin.FetchRequest) (*http.Response, error)
}

func zcodeResetAccountOf(id, user string) (zcodeResetAccount, error) {
	pp, ok := PluginOf(id)
	if !ok || pp.ID != "zcode" || strings.TrimSpace(user) == "" {
		return zcodeResetAccount{}, errors.New("name the ZCode provider and account whose reset is used")
	}
	var found *pluginLogin
	logins := pluginLogins(pp)
	for i := range logins {
		l := &logins[i]
		if l.acct.Key == user || strings.EqualFold(l.User, user) {
			if found != nil && found.acct.Key != l.acct.Key {
				return zcodeResetAccount{}, errors.New("the ZCode account name is ambiguous")
			}
			found = l
		}
	}
	if found == nil {
		return zcodeResetAccount{}, errors.New("no such ZCode account")
	}
	return zcodeResetAccount{provider: pp.ID, key: found.acct.Key, user: found.User}, nil
}

func validResetBinding(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

func validZCodeResetWindow(window string) bool { return window == "fiveHour" || window == "weekly" }

type zcodeResetError struct {
	kind       string
	code       float64
	status     int
	dispatched bool
}

func (e *zcodeResetError) Error() string {
	if e.code != 0 {
		return fmt.Sprintf("ZCode reset refused (code %g)", e.code)
	}
	if e.status != 0 {
		return fmt.Sprintf("ZCode reset %s (HTTP %d)", e.kind, e.status)
	}
	return "ZCode reset " + e.kind
}

func (e *zcodeResetError) transient() bool {
	return e.kind == "network" || e.kind == "cancelled" || e.code == 2007 || e.status >= 500 && e.status < 600
}

type zcodeResetReply struct {
	scope   string
	context string
	team    bool
	code    float64
	data    json.RawMessage
}

func (a zcodeResetAccount) call(ctx context.Context, operation, expected string, body any) (zcodeResetReply, error) {
	var reply zcodeResetReply
	method := http.MethodPost
	if operation == "status" {
		method = http.MethodGet
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return reply, err
		}
	}
	headers := map[string]string{"accept": "application/json"}
	if expected != "" {
		headers["x-magpie-zcode-reset-context"] = expected
	}
	if data != nil {
		headers["content-type"] = "application/json"
	}
	ctx = ViaLogin(ctx, "plugin:zcode", a.user)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	via := a.via
	if via == nil {
		via = plugin.Fetch
	}
	res, err := via(ctx, plugin.FetchRequest{Provider: a.provider, Account: a.key, URL: zcodeResetURL + operation, Method: method, Headers: headers, Body: data})
	if err != nil {
		return reply, &zcodeResetError{kind: "network", dispatched: method == http.MethodPost}
	}
	defer res.Body.Close()
	dispatched := res.Header.Get("X-Magpie-ZCode-Reset-Dispatched") == "1"
	if kind := res.Header.Get("X-Magpie-ZCode-Reset-Error"); kind != "" {
		return reply, &zcodeResetError{kind: kind, status: res.StatusCode, dispatched: dispatched}
	}
	reply.scope, reply.context = res.Header.Get(zcodeResetScopeHeader), res.Header.Get(zcodeResetContextHeader)
	target := res.Header.Get("X-Magpie-ZCode-Reset-Target")
	if !validResetBinding(reply.scope) || !validResetBinding(reply.context) || target != "PERSONAL" && target != "TEAM" {
		return reply, &zcodeResetError{kind: "not supported by this plugin", dispatched: dispatched}
	}
	if expected != "" && reply.context != expected {
		return reply, &zcodeResetError{kind: "account_changed", dispatched: dispatched}
	}
	reply.team = target == "TEAM"
	b, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if readErr != nil {
		return reply, &zcodeResetError{kind: "network", dispatched: dispatched}
	}
	var envelope struct {
		Code *float64        `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	parseErr := json.Unmarshal(b, &envelope)
	// The vendor's nonzero business code has precedence over HTTP status.
	if parseErr == nil && envelope.Code != nil && *envelope.Code != 0 {
		reply.code, reply.data = *envelope.Code, envelope.Data
		if operation == "opportunity" && reply.code == 3301 {
			return reply, nil
		}
		return reply, &zcodeResetError{kind: "business refusal", code: reply.code, dispatched: dispatched}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		kind := "HTTP refusal"
		if operation == "opportunity" && res.StatusCode == 429 {
			kind = "throttled"
		}
		return reply, &zcodeResetError{kind: kind, status: res.StatusCode, dispatched: dispatched}
	}
	if parseErr != nil || envelope.Code == nil {
		return reply, &zcodeResetError{kind: "invalid response", dispatched: dispatched}
	}
	reply.data = envelope.Data
	return reply, nil
}

type zcodeResetCredit struct {
	Expire float64 `json:"expire_at"`
}

type zcodeResetStatus struct {
	fiveHour     []zcodeResetCredit
	weekly       []zcodeResetCredit
	fiveHourUsed float64
	weeklyUsed   float64
	unread       bool
}

func (s zcodeResetStatus) latest(window string) float64 {
	if window == "fiveHour" {
		return s.fiveHourUsed
	}
	return s.weeklyUsed
}

func (s zcodeResetStatus) available(window string) int {
	if window == "fiveHour" {
		return len(s.fiveHour)
	}
	return len(s.weekly)
}

func positiveResetTime(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func decodeZCodeResetStatus(data json.RawMessage) (zcodeResetStatus, error) {
	var status zcodeResetStatus
	var wire struct {
		FiveHour        *[]zcodeResetCredit `json:"available_five_hour_resets"`
		Weekly          *[]zcodeResetCredit `json:"available_week_resets"`
		FiveHourHistory json.RawMessage     `json:"latest_five_hour_reset_history"`
		WeeklyHistory   json.RawMessage     `json:"latest_week_reset_history"`
		Unread          *bool               `json:"has_unread_history"`
	}
	invalid := func() (zcodeResetStatus, error) {
		return zcodeResetStatus{}, &zcodeResetError{kind: "invalid response"}
	}
	if json.Unmarshal(data, &wire) != nil || wire.FiveHour == nil || wire.Weekly == nil || wire.Unread == nil {
		return invalid()
	}
	readHistory := func(raw json.RawMessage) (float64, bool) {
		if string(raw) == "null" {
			return 0, true
		}
		var history struct {
			Used float64 `json:"used_at"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &history) != nil || !positiveResetTime(history.Used) {
			return 0, false
		}
		return history.Used, true
	}
	var ok bool
	if status.fiveHourUsed, ok = readHistory(wire.FiveHourHistory); !ok {
		return invalid()
	}
	if status.weeklyUsed, ok = readHistory(wire.WeeklyHistory); !ok {
		return invalid()
	}
	for _, item := range *wire.FiveHour {
		if !positiveResetTime(item.Expire) {
			return invalid()
		}
	}
	for _, item := range *wire.Weekly {
		if !positiveResetTime(item.Expire) {
			return invalid()
		}
	}
	status.fiveHour, status.weekly = *wire.FiveHour, *wire.Weekly
	status.unread = *wire.Unread
	return status, nil
}

func (a zcodeResetAccount) status(ctx context.Context, expected string) (zcodeResetReply, zcodeResetStatus, error) {
	reply, err := a.call(ctx, "status", expected, nil)
	if err != nil {
		return reply, zcodeResetStatus{}, err
	}
	status, err := decodeZCodeResetStatus(reply.data)
	return reply, status, err
}

// The state is independent of logins: replacing a sign-in must not erase an
// unanswered financial operation. A missing file is new; unreadable is unknown.
type zcodePendingReset struct {
	Window    string    `json:"window"`
	Key       string    `json:"key"`
	Baseline  float64   `json:"baseline"`
	Started   time.Time `json:"started"`
	Claim     string    `json:"claim,omitempty"`
	ClaimedAt time.Time `json:"claimedAt,omitempty"`
}

type zcodeResetRecord struct {
	Seen               bool               `json:"seen"`
	FiveHourUsed       float64            `json:"fiveHourUsed"`
	WeeklyUsed         float64            `json:"weeklyUsed"`
	FiveHourConfirmed  time.Time          `json:"fiveHourConfirmed,omitempty"`
	WeeklyConfirmed    time.Time          `json:"weeklyConfirmed,omitempty"`
	OpportunityKey     string             `json:"opportunityKey,omitempty"`
	OpportunityAt      time.Time          `json:"opportunityAt,omitempty"`
	OpportunityClaim   string             `json:"opportunityClaim,omitempty"`
	OpportunityStarted time.Time          `json:"opportunityStarted,omitempty"`
	Pending            *zcodePendingReset `json:"pending,omitempty"`
}

type zcodeResetStore struct {
	Version int                         `json:"version"`
	Scopes  map[string]zcodeResetRecord `json:"scopes"`
}

func zcodeResetStatePath() string { return filepath.Join(settings.Dir(), "zcode-resets.json") }

func withZCodeResetState(ctx context.Context, scope string, change func(*zcodeResetRecord) (bool, error)) error {
	if !validResetBinding(scope) {
		return errors.New("invalid ZCode reset scope")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wait := time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
		wait = max(0, time.Until(deadline))
	}
	path := zcodeResetStatePath()
	unlock, held := filelock.Lock(path+".lock", wait)
	if !held {
		return errors.New("could not lock ZCode reset state")
	}
	defer unlock()
	b, err := os.ReadFile(path)
	state := zcodeResetStore{Version: 1, Scopes: map[string]zcodeResetRecord{}}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if json.Unmarshal(b, &state) != nil || state.Version != 1 || state.Scopes == nil {
			return errors.New("ZCode reset state is unreadable")
		}
		for key, record := range state.Scopes {
			if !validResetBinding(key) || !validZCodeResetRecord(record) {
				return errors.New("ZCode reset state is invalid")
			}
		}
	}
	record := state.Scopes[scope]
	changed, err := change(&record)
	if err != nil || !changed {
		return err
	}
	state.Scopes[scope] = record
	b, err = json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return steady.WriteFile(path, append(b, '\n'), 0o600)
}

func validZCodeResetRecord(record zcodeResetRecord) bool {
	if record.FiveHourUsed < 0 || record.WeeklyUsed < 0 || math.IsInf(record.FiveHourUsed, 0) || math.IsInf(record.WeeklyUsed, 0) {
		return false
	}
	if len(record.OpportunityKey) > 64 || record.OpportunityClaim != "" && (record.OpportunityKey == "" || record.OpportunityStarted.IsZero()) {
		return false
	}
	if p := record.Pending; p != nil {
		if !validZCodeResetWindow(p.Window) || p.Key == "" || len(p.Key) > 64 || p.Baseline < 0 || math.IsInf(p.Baseline, 0) || p.Started.IsZero() || p.Claim != "" && p.ClaimedAt.IsZero() {
			return false
		}
	}
	return true
}

func observeZCodeReset(record *zcodeResetRecord, status zcodeResetStatus) (changed, completed bool) {
	if record.Pending != nil && status.latest(record.Pending.Window) > record.Pending.Baseline {
		if record.Pending.Window == "fiveHour" {
			record.FiveHourConfirmed = zcodeResetNow().UTC()
		} else {
			record.WeeklyConfirmed = zcodeResetNow().UTC()
		}
		record.Pending = nil
		changed, completed = true, true
	}
	if status.fiveHourUsed > record.FiveHourUsed || status.weeklyUsed > record.WeeklyUsed {
		completed = completed || record.Seen
		record.FiveHourUsed = max(record.FiveHourUsed, status.fiveHourUsed)
		record.WeeklyUsed = max(record.WeeklyUsed, status.weeklyUsed)
		changed = true
	}
	if !record.Seen {
		record.Seen, changed = true, true
	}
	return changed, completed
}

func resetStateFailure(err error) string { return "ZCode resets unavailable: " + err.Error() }

// fetchZCodeResets runs on the existing usage refresh, never on a new timer.
// Reset errors are returned separately and cannot erase the real quota windows.
func fetchZCodeResets(ctx context.Context, a zcodeResetAccount, scope, expected string) (*ResetCredits, string) {
	reply, status, err := a.status(ctx, expected)
	if err != nil {
		return nil, resetStateFailure(err)
	}
	if reply.scope != scope {
		return nil, "ZCode reset account changed; refresh its card"
	}
	return zcodeResetSnapshot(ctx, a, reply, status)
}

// ReadZCodeResets captures the selection before a CLI confirmation. The
// subsequent spend must still match this context, not a newly selected plan.
func ReadZCodeResets(ctx context.Context, id, user string) (*ResetCredits, error) {
	a, err := zcodeResetAccountOf(id, user)
	if err != nil {
		return nil, err
	}
	reply, status, err := a.status(ctx, "")
	if err != nil {
		return nil, err
	}
	resets, why := zcodeResetSnapshot(ctx, a, reply, status)
	if why != "" {
		return resets, errors.New(why)
	}
	return resets, nil
}

func zcodeResetSnapshot(ctx context.Context, a zcodeResetAccount, reply zcodeResetReply, status zcodeResetStatus) (*ResetCredits, string) {
	resets := zcodeResetCredits(reply, status)
	var completed bool
	err := withZCodeResetState(ctx, reply.scope, func(record *zcodeResetRecord) (bool, error) {
		changed, done := observeZCodeReset(record, status)
		completed = done
		if record.Pending != nil {
			resets.Pending = record.Pending.Window
		}
		return changed, nil
	})
	if err != nil {
		resets.CanUse = false
		return resets, resetStateFailure(err)
	}
	if completed {
		_, _ = a.call(ctx, "history/read", reply.context, nil)
	} else if resets.Pending == "" {
		requestZCodeResetOpportunity(ctx, a, reply)
	}
	return resets, ""
}

func zcodeResetCredits(reply zcodeResetReply, status zcodeResetStatus) *ResetCredits {
	count := len(status.fiveHour) + len(status.weekly)
	r := &ResetCredits{Count: count, ByWindow: true, FiveHour: len(status.fiveHour), Weekly: len(status.weekly), Team: reply.team, CanUse: true, Context: reply.context, Each: make([]ResetCard, 0, count)}
	add := func(window string, expiry float64) {
		// JSON timestamps outside a representable year remain unknown; they
		// must never make an optional reset break the authoritative quota.
		if expiry > 253402300799999 {
			r.untilUnread = true
			return
		}
		t := time.UnixMilli(int64(expiry)).UTC()
		r.Each = append(r.Each, ResetCard{Window: window, Until: &t})
		if r.Until == nil || t.Before(*r.Until) {
			r.Until = &t
		}
	}
	for _, credit := range status.fiveHour {
		add("fiveHour", credit.Expire)
	}
	for _, credit := range status.weekly {
		add("weekly", credit.Expire)
	}
	sortResetCards(r.Each)
	return r
}

func requestZCodeResetOpportunity(ctx context.Context, a zcodeResetAccount, reply zcodeResetReply) {
	now := zcodeResetNow().UTC()
	var key, claim string
	err := withZCodeResetState(ctx, reply.scope, func(record *zcodeResetRecord) (bool, error) {
		if record.Pending != nil || now.Before(record.OpportunityAt) || record.OpportunityClaim != "" && now.Sub(record.OpportunityStarted) < zcodeResetClaimWait {
			return false, nil
		}
		if record.OpportunityKey == "" {
			record.OpportunityKey = newRedeemID()
		}
		key, claim = record.OpportunityKey, newRedeemID()
		record.OpportunityClaim, record.OpportunityStarted = claim, now
		return true, nil
	})
	if err != nil || claim == "" {
		return
	}
	result, callErr := a.call(ctx, "opportunity", reply.context, map[string]string{"idempotency_key": key})
	var grant struct {
		Granted *bool    `json:"granted"`
		NextTry *float64 `json:"next_try_at"`
	}
	if callErr == nil {
		if json.Unmarshal(result.data, &grant) != nil || grant.Granted == nil || result.code == 3301 && (*grant.Granted || grant.NextTry == nil || !positiveResetTime(*grant.NextTry) || math.Trunc(*grant.NextTry) != *grant.NextTry) || result.code == 0 && !*grant.Granted {
			callErr = &zcodeResetError{kind: "invalid response"}
		}
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = withZCodeResetState(finish, reply.scope, func(record *zcodeResetRecord) (bool, error) {
		if record.OpportunityClaim != claim || record.OpportunityKey != key {
			return false, nil
		}
		record.OpportunityClaim, record.OpportunityStarted = "", time.Time{}
		now := zcodeResetNow().UTC()
		record.OpportunityAt = now.Add(10 * time.Minute)
		if callErr == nil && result.code == 3301 {
			record.OpportunityAt = now.Add(5 * time.Minute)
			if next := time.UnixMilli(int64(*grant.NextTry)); next.After(record.OpportunityAt) {
				record.OpportunityAt = next
			}
		} else if callErr != nil {
			var denied *zcodeResetError
			// The opportunity coordinator retries only transport failures
			// and business 2007; HTTP and other business refusals are stable.
			if errors.As(callErr, &denied) && (denied.kind == "network" || denied.kind == "cancelled" || denied.code == 2007) {
				record.OpportunityAt = now.Add(5 * time.Minute)
				return true, nil // retry the SAME opportunity key
			}
		}
		record.OpportunityKey = ""
		return true, nil
	})
}

// SpendZCodeReset is the single owner used by the GUI, CLI and remote API.
// It reconciles an unanswered use before authorizing any fresh debit.
func SpendZCodeReset(ctx context.Context, id, user, window, expected string) (ResetOutcome, error) {
	if !validZCodeResetWindow(window) {
		return ResetOutcome{}, errors.New("choose the five-hour or weekly ZCode reset")
	}
	if !validResetBinding(expected) {
		return ResetOutcome{}, errors.New("refresh the ZCode account before using a reset")
	}
	a, err := zcodeResetAccountOf(id, user)
	if err != nil {
		return ResetOutcome{}, err
	}
	return spendZCodeReset(ctx, a, window, expected)
}

func spendZCodeReset(ctx context.Context, a zcodeResetAccount, window, expected string) (ResetOutcome, error) {
	began := zcodeResetNow().UTC()
	reply, status, err := a.status(ctx, expected)
	if err != nil {
		return ResetOutcome{}, err
	}
	now := zcodeResetNow().UTC()
	var pending zcodePendingReset
	var adopted, waiting bool
	err = withZCodeResetState(ctx, reply.scope, func(record *zcodeResetRecord) (bool, error) {
		prior := record.Pending
		changed, _ := observeZCodeReset(record, status)
		if prior != nil && prior.Window == window && record.Pending == nil {
			adopted = true
			return true, nil
		}
		seen := record.WeeklyUsed
		confirmedAt := record.WeeklyConfirmed
		if window == "fiveHour" {
			seen, confirmedAt = record.FiveHourUsed, record.FiveHourConfirmed
		}
		if status.latest(window) < seen || !confirmedAt.IsZero() && !confirmedAt.Before(began) {
			adopted = true
			return changed, nil
		}
		if record.Pending != nil {
			if record.Pending.Window != window || record.Pending.Claim != "" && now.Sub(record.Pending.ClaimedAt) < zcodeResetClaimWait {
				waiting = true
				return changed, nil
			}
		} else {
			if status.available(window) == 0 {
				return changed, nil
			}
			record.Pending = &zcodePendingReset{Window: window, Key: newRedeemID(), Baseline: status.latest(window), Started: now}
		}
		record.Pending.Claim, record.Pending.ClaimedAt = newRedeemID(), now
		pending = *record.Pending
		return true, nil
	})
	if err != nil {
		return ResetOutcome{}, err
	}
	if adopted {
		_, _ = a.call(ctx, "history/read", expected, nil)
		StaleAllowance("plugin:"+a.provider, a.user)
		toldUsage()
		return ResetOutcome{Code: "already_used", Windows: 1}, nil
	}
	if waiting {
		return ResetOutcome{Code: "reset_unconfirmed"}, nil
	}
	if pending.Key == "" {
		return ResetOutcome{Code: "no_credit"}, nil
	}
	kind := "FIVE_HOUR"
	if window == "weekly" {
		kind = "WEEK"
	}
	result, callErr := a.call(ctx, "use", expected, map[string]string{"idempotency_key": pending.Key, "reset_type": kind})
	if callErr == nil {
		var accepted struct {
			Used *bool `json:"used"`
		}
		if json.Unmarshal(result.data, &accepted) != nil || accepted.Used == nil || !*accepted.Used {
			callErr = &zcodeResetError{kind: "invalid response", dispatched: true}
		}
	}
	confirmed := false
	definite := false
	if callErr != nil {
		var refused *zcodeResetError
		definite = errors.As(callErr, &refused) && (refused.code != 0 && !refused.transient() || !refused.dispatched && (refused.kind == "invalid_request" || refused.kind == "unavailable"))
	}
	if callErr == nil {
		for _, delay := range []time.Duration{0, 250 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond} {
			if delay != 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					break
				}
			}
			if ctx.Err() != nil {
				break
			}
			nextReply, next, readErr := a.status(ctx, expected)
			if readErr == nil && nextReply.scope == reply.scope && next.latest(window) > pending.Baseline {
				confirmed = true
				status = next
				break
			}
		}
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	persistErr := withZCodeResetState(finish, reply.scope, func(record *zcodeResetRecord) (bool, error) {
		if record.Pending == nil || record.Pending.Key != pending.Key || record.Pending.Claim != pending.Claim {
			return false, nil
		}
		if confirmed {
			observeZCodeReset(record, status)
		} else if definite {
			record.Pending = nil
		} else {
			record.Pending.Claim, record.Pending.ClaimedAt = "", time.Time{}
		}
		return true, nil
	})
	if !confirmed {
		if persistErr != nil {
			return ResetOutcome{Code: "reset_unconfirmed"}, persistErr
		}
		if definite {
			return ResetOutcome{}, callErr
		}
		return ResetOutcome{Code: "reset_unconfirmed"}, nil
	}
	if persistErr != nil {
		log.Printf("ZCode reset confirmed, but recording its completion failed: %v", persistErr)
	}
	_, _ = a.call(ctx, "history/read", expected, nil)
	StaleAllowance("plugin:"+a.provider, a.user)
	subscriptionUsageCache.Lock()
	subscriptionUsageCache.at, subscriptionUsageCache.data = time.Time{}, nil
	subscriptionUsageCache.Unlock()
	toldUsage()
	return ResetOutcome{Code: "reset", Windows: 1}, nil
}
