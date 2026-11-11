package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

type zcodeResetHistory struct {
	UsedAt float64 `json:"used_at"`
}

type zcodeResetWire struct {
	FiveHour []struct {
		ExpireAt float64 `json:"expire_at"`
	} `json:"available_five_hour_resets"`
	Week []struct {
		ExpireAt float64 `json:"expire_at"`
	} `json:"available_week_resets"`
	LatestFive *zcodeResetHistory `json:"latest_five_hour_reset_history"`
	LatestWeek *zcodeResetHistory `json:"latest_week_reset_history"`
	Unread     bool               `json:"has_unread_history"`
}

type zcodeResetClaim struct {
	Key        string  `json:"key"`
	Window     string  `json:"window"`
	Generation string  `json:"generation"`
	Baseline   float64 `json:"baseline"`
	HighWater  float64 `json:"highWater"`
	Accepted   bool    `json:"accepted,omitempty"`
	ObservedAt float64 `json:"observedAt,omitempty"`
}

type zcodeResetEvent struct {
	Window string  `json:"window"`
	UsedAt float64 `json:"usedAt"`
}

type zcodeResetJournal struct {
	FiveHigh       float64           `json:"fiveHigh,omitempty"`
	WeekHigh       float64           `json:"weekHigh,omitempty"`
	Pending        *zcodeResetClaim  `json:"pending,omitempty"`
	Last           *zcodeResetEvent  `json:"last,omitempty"`
	Events         []zcodeResetEvent `json:"events,omitempty"`
	AckPending     bool              `json:"ackPending,omitempty"`
	OpportunityKey string            `json:"opportunityKey,omitempty"`
	OpportunityAt  int64             `json:"opportunityAt,omitempty"`
}

// Each host notices completions another magpie persisted, even if it did
// not itself observe the vendor response that advanced the high-water mark.
var zcodeResetSeen struct {
	sync.Mutex
	m map[string][2]float64
}

func zcodeNoticeCompletion(c plugin.AccountContext, providerID, account string, r zcodeResetJournal) bool {
	key := c.Origin + "\x00" + c.Scope
	zcodeResetSeen.Lock()
	defer zcodeResetSeen.Unlock()
	seen := zcodeResetSeen.m[key]
	if r.FiveHigh <= seen[0] && r.WeekHigh <= seen[1] {
		return false
	}
	l, ok := zcodeLogin(providerID, account)
	if !ok {
		return false
	}
	resetQuotaReadBarrier(l)
	if zcodeResetSeen.m == nil {
		zcodeResetSeen.m = map[string][2]float64{}
	}
	zcodeResetSeen.m[key] = [2]float64{max(seen[0], r.FiveHigh), max(seen[1], r.WeekHigh)}
	return true
}

func (j zcodeResetJournal) valid() bool {
	if j.FiveHigh < 0 || j.WeekHigh < 0 || j.OpportunityAt < 0 || len(j.OpportunityKey) > 64 {
		return false
	}
	validEvent := func(e zcodeResetEvent) bool { return (e.Window == "five-hour" || e.Window == "week") && e.UsedAt > 0 }
	if j.Last != nil && !validEvent(*j.Last) {
		return false
	}
	for _, e := range j.Events {
		if !validEvent(e) {
			return false
		}
	}
	if p := j.Pending; p != nil {
		high := j.FiveHigh
		if p.Window == "week" {
			high = j.WeekHigh
		}
		return p.Key != "" && len(p.Key) <= 64 && p.Generation != "" && (p.Window == "five-hour" || p.Window == "week") &&
			p.Baseline >= 0 && p.HighWater >= 0 && p.ObservedAt >= 0 &&
			(p.ObservedAt == 0 || p.ObservedAt > p.Baseline && p.ObservedAt > p.HighWater && p.ObservedAt <= high)
	}
	return true
}

// ZCodeResetState is official reset availability, distinct from a legacy
// reset-list observation. A pending outcome is never converted to success.
type ZCodeResetState struct {
	Source            string           `json:"source"`
	Available         bool             `json:"available"`
	Error             string           `json:"error,omitempty"`
	FiveHour          []float64        `json:"fiveHour,omitempty"`
	Week              []float64        `json:"week,omitempty"`
	Pending           string           `json:"pending,omitempty"`
	PendingWindow     string           `json:"pendingWindow,omitempty"`
	ObservedAt        float64          `json:"observedAt,omitempty"`
	NextOpportunityAt int64            `json:"nextOpportunityAt,omitempty"`
	Last              *zcodeResetEvent `json:"last,omitempty"`
}

func resetState(w zcodeResetWire, j zcodeResetJournal) ZCodeResetState {
	out := ZCodeResetState{Source: "zcode", Available: true, NextOpportunityAt: j.OpportunityAt, Last: j.Last}
	now := float64(time.Now().UnixMilli())
	for _, card := range w.FiveHour {
		if card.ExpireAt > now {
			out.FiveHour = append(out.FiveHour, card.ExpireAt)
		}
	}
	for _, card := range w.Week {
		if card.ExpireAt > now {
			out.Week = append(out.Week, card.ExpireAt)
		}
	}
	if p := j.Pending; p != nil {
		out.Pending, out.PendingWindow, out.ObservedAt = "unconfirmed", p.Window, p.ObservedAt
		if p.ObservedAt > 0 {
			out.Pending = "observed-completion"
		}
	}
	return out
}

func resetLatest(h *zcodeResetHistory) float64 {
	if h == nil {
		return 0
	}
	return h.UsedAt
}

func validResetWire(w zcodeResetWire) bool {
	if w.LatestFive != nil && w.LatestFive.UsedAt <= 0 || w.LatestWeek != nil && w.LatestWeek.UsedAt <= 0 {
		return false
	}
	for _, c := range w.FiveHour {
		if c.ExpireAt <= 0 {
			return false
		}
	}
	for _, c := range w.Week {
		if c.ExpireAt <= 0 {
			return false
		}
	}
	return true
}

func zcodeResetRead(ctx context.Context, providerID, account string, c plugin.AccountContext) (zcodeResetWire, ZCodeResetState, bool, error) {
	var wire zcodeResetWire
	var before zcodeResetJournal
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		before = j.Reset
		return false, nil
	}); err != nil {
		return wire, ZCodeResetState{}, false, err
	}
	localCompletion := zcodeNoticeCompletion(c, providerID, account, before)
	data, err := zcodeAccountCall(ctx, providerID, account, c, "reset.status", nil)
	if err != nil {
		return wire, ZCodeResetState{}, false, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil ||
		len(fields["available_five_hour_resets"]) == 0 || len(fields["available_week_resets"]) == 0 ||
		len(fields["latest_five_hour_reset_history"]) == 0 || len(fields["latest_week_reset_history"]) == 0 ||
		len(fields["has_unread_history"]) == 0 || string(fields["has_unread_history"]) == "null" ||
		json.Unmarshal(data, &wire) != nil || wire.FiveHour == nil || wire.Week == nil || !validResetWire(wire) {
		return wire, ZCodeResetState{}, false, errors.New("invalid native reset status")
	}
	var snapshot zcodeResetJournal
	newEvent := false
	legacyDebt := false
	err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		legacyDebt = j.LegacyResetDebt
		r := &j.Reset
		hadPending := r.Pending != nil
		observe := func(window string, used float64, high *float64) {
			if used <= *high {
				return
			}
			*high = used
			e := zcodeResetEvent{Window: window, UsedAt: used}
			r.Events = append(r.Events, e)
			if len(r.Events) > 64 {
				r.Events = append([]zcodeResetEvent(nil), r.Events[len(r.Events)-64:]...)
			}
			r.Last = &e
			newEvent = true
			if p := r.Pending; p != nil && p.Window == window && used > p.Baseline && used > p.HighWater {
				p.ObservedAt = used
			}
		}
		observe("five-hour", resetLatest(wire.LatestFive), &r.FiveHigh)
		observe("week", resetLatest(wire.LatestWeek), &r.WeekHigh)
		if wire.Unread && (newEvent || r.AckPending || resetLatest(wire.LatestFive) > 0 || resetLatest(wire.LatestWeek) > 0) {
			r.AckPending = true
		}
		if p := r.Pending; p != nil && p.Accepted && p.ObservedAt > 0 {
			r.Pending = nil
		}
		snapshot = *r
		return newEvent || r.AckPending || hadPending && r.Pending == nil, nil
	})
	if err != nil {
		return wire, ZCodeResetState{}, false, err
	}
	localCompletion = zcodeNoticeCompletion(c, providerID, account, snapshot) || localCompletion
	if snapshot.AckPending {
		if _, err := zcodeAccountCall(ctx, providerID, account, c, "reset.history.read", nil); err == nil {
			_ = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
				// Do not clear a newer observation that arrived during the ack.
				if j.Reset.FiveHigh != snapshot.FiveHigh || j.Reset.WeekHigh != snapshot.WeekHigh {
					return false, nil
				}
				j.Reset.AckPending = false
				return true, nil
			})
		}
	}
	state := resetState(wire, snapshot)
	if legacyDebt && state.Pending == "" {
		state.Pending = "legacy-scope-unresolved"
	}
	return wire, state, newEvent || localCompletion, nil
}

func zcodeOpportunity(ctx context.Context, providerID, account string, c plugin.AccountContext) (bool, error) {
	unlock, err := zcodeFence(c, "reset")
	if err != nil {
		// A concurrent owner is not a reason to start a second POST.
		return false, nil
	}
	defer unlock()
	newKey, err := zcodeJournalKey()
	if err != nil {
		return false, err
	}
	key := ""
	now := time.Now().UnixMilli()
	err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		r := &j.Reset
		if j.LegacyResetDebt {
			return false, errors.New("an older ZCode reset has unresolved account scope; no new reset may be requested")
		}
		if r.Pending != nil || now < r.OpportunityAt {
			return false, nil
		}
		key = r.OpportunityKey
		if key == "" {
			key = newKey
		}
		r.OpportunityKey, r.OpportunityAt = key, now+int64((5*time.Minute)/time.Millisecond)
		return true, nil
	})
	if err != nil || key == "" {
		return false, err
	}
	data, callErr := zcodeAccountCall(ctx, providerID, account, c, "reset.opportunity", map[string]any{"idempotency_key": key})
	var result struct {
		Granted   *bool `json:"granted"`
		NextTryAt int64 `json:"next_try_at"`
	}
	if callErr == nil && (json.Unmarshal(data, &result) != nil || result.Granted == nil || !*result.Granted && result.NextTryAt <= 0) {
		callErr = errors.New("invalid reset opportunity response")
	}
	granted := callErr == nil && *result.Granted
	finishErr := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		r := &j.Reset
		if r.OpportunityKey != key {
			return false, errors.New("opportunity claim changed during the request")
		}
		now := time.Now().UnixMilli()
		switch {
		case granted:
			r.OpportunityKey, r.OpportunityAt = "", now+int64((10*time.Minute)/time.Millisecond)
		case callErr == nil:
			r.OpportunityKey, r.OpportunityAt = "", max(result.NextTryAt, now+int64((5*time.Minute)/time.Millisecond))
		default:
			var ae *plugin.AccountError
			var network net.Error
			if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(callErr, context.Canceled) ||
				errors.Is(callErr, io.EOF) || errors.Is(callErr, io.ErrUnexpectedEOF) || errors.As(callErr, &network) ||
				errors.As(callErr, &ae) && !ae.Throttled && (ae.Retryable || ae.Code == 2007) {
				r.OpportunityAt = now + int64((5*time.Minute)/time.Millisecond)
			} else {
				r.OpportunityKey, r.OpportunityAt = "", now+int64((10*time.Minute)/time.Millisecond)
			}
		}
		return true, nil
	})
	if finishErr != nil {
		return false, finishErr
	}
	return granted, callErr
}

// ZCodeResetStatus reads the selected account's official reset service.
// Opportunity is evaluated only when requested by an allowed usage refresh.
func ZCodeResetStatus(ctx context.Context, providerID, account string, opportunity bool) (ZCodeResetState, error) {
	key, err := zcodeAccountKey(providerID, account)
	if err != nil {
		return ZCodeResetState{}, err
	}
	c, err := zcodeContext(ctx, providerID, key)
	if err != nil {
		return ZCodeResetState{}, err
	}
	if c.Kind != "individual" && c.Kind != "team" {
		return ZCodeResetState{}, errors.New("native reset is unavailable for this account; sign in to a Coding Plan")
	}
	return zcodeResetStatus(ctx, providerID, key, c, opportunity)
}

func zcodeResetStatus(ctx context.Context, providerID, key string, c plugin.AccountContext, opportunity bool) (ZCodeResetState, error) {
	_, state, observed, err := zcodeResetRead(ctx, providerID, key, c)
	if err != nil {
		var saved zcodeResetJournal
		legacyDebt := false
		if journalErr := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
			saved = j.Reset
			legacyDebt = j.LegacyResetDebt
			return false, nil
		}); journalErr == nil {
			state = resetState(zcodeResetWire{}, saved)
			if legacyDebt && state.Pending == "" {
				state.Pending = "legacy-scope-unresolved"
			}
			state.Available, state.Error = false, err.Error()
		}
		return state, err
	}
	if !opportunity || observed || state.Pending != "" || state.NextOpportunityAt > time.Now().UnixMilli() {
		return state, err
	}
	granted, err := zcodeOpportunity(ctx, providerID, key, c)
	if err != nil {
		return state, nil // optional opportunity failure never hides an authoritative status
	}
	if granted {
		_, refreshed, _, readErr := zcodeResetRead(ctx, providerID, key, c)
		if readErr != nil {
			state.Available, state.Error = false, readErr.Error()
			return state, readErr
		}
		state = refreshed
	}
	return state, nil
}

// UseZCodeReset requires an explicit account and five-hour or week window.
// An accepted POST is still unconfirmed until newer history is observed.
func UseZCodeReset(ctx context.Context, providerID, account, window string) (ZCodeResetState, error) {
	expectation, ok := expectedZCodeAccount(ctx)
	if !ok || expectation.scope == "" || expectation.generation == "" {
		return ZCodeResetState{}, errors.New("capture the ZCode account scope and generation before confirming a reset")
	}
	if window != "five-hour" && window != "week" {
		return ZCodeResetState{}, errors.New("reset window must be five-hour or week")
	}
	key, err := zcodeAccountKey(providerID, account)
	if err != nil {
		return ZCodeResetState{}, err
	}
	c, err := zcodeContext(ctx, providerID, key)
	if err != nil {
		return ZCodeResetState{}, err
	}
	if c.Kind != "individual" && c.Kind != "team" {
		return ZCodeResetState{}, errors.New("native reset is unavailable for this account; sign in to a Coding Plan")
	}
	return useZCodeReset(ctx, providerID, key, c, window)
}

func useZCodeReset(ctx context.Context, providerID, key string, c plugin.AccountContext, window string) (ZCodeResetState, error) {
	unlock, err := zcodeFence(c, "reset")
	if err != nil {
		return ZCodeResetState{}, err
	}
	defer unlock()
	wire, state, _, err := zcodeResetRead(ctx, providerID, key, c)
	if err != nil {
		return state, err
	}
	if window == "five-hour" && len(state.FiveHour) == 0 || window == "week" && len(state.Week) == 0 {
		return state, errors.New("no unexpired reset is available for that window")
	}
	claim, err := zcodeJournalKey()
	if err != nil {
		return state, err
	}
	err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		if j.LegacyResetDebt {
			return false, errors.New("an older ZCode reset has unresolved account scope; no new reset may be spent")
		}
		if j.Reset.Pending != nil {
			return false, errors.New("a previous reset has an unconfirmed outcome; inspect and resolve it before spending another")
		}
		baseline, high := resetLatest(wire.LatestFive), j.Reset.FiveHigh
		if window == "week" {
			baseline, high = resetLatest(wire.LatestWeek), j.Reset.WeekHigh
		}
		j.Reset.Pending = &zcodeResetClaim{Key: claim, Window: window, Generation: c.Generation, Baseline: baseline, HighWater: high}
		return true, nil
	})
	if err != nil {
		return state, err
	}
	typ := "FIVE_HOUR"
	if window == "week" {
		typ = "WEEK"
	}
	data, callErr := zcodeAccountCall(ctx, providerID, key, c, "reset.use", map[string]any{"idempotency_key": claim, "reset_type": typ})
	var result struct {
		Used bool `json:"used"`
	}
	if callErr == nil && (json.Unmarshal(data, &result) != nil || !result.Used) {
		callErr = errors.New("invalid reset use response; outcome is unknown")
	}
	updateErr := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		p := j.Reset.Pending
		if p == nil || p.Key != claim || p.Generation != c.Generation {
			return false, errors.New("reset claim changed during use")
		}
		if callErr == nil {
			p.Accepted = true
			if p.ObservedAt > 0 {
				j.Reset.Pending = nil
			}
		}
		return true, nil
	})
	if updateErr != nil {
		return state, fmt.Errorf("reset outcome requires reconciliation: %w", updateErr)
	}
	if callErr != nil {
		return ZCodeResetState{Source: "zcode", Available: true, Pending: "unconfirmed", PendingWindow: window},
			fmt.Errorf("reset outcome is unknown and it may have been used; inspect or resolve it before trying again: %w", callErr)
	}
	state.Pending, state.PendingWindow = "unconfirmed", window
	for _, delay := range []time.Duration{0, 250 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond} {
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ZCodeResetState{Source: "zcode", Available: true, Pending: "unconfirmed", PendingWindow: window}, ctx.Err()
			}
		}
		_, next, _, readErr := zcodeResetRead(ctx, providerID, key, c)
		if readErr != nil {
			continue
		}
		state = next
		if state.Pending == "" {
			return state, nil
		}
	}
	return state, nil
}

// ResolveZCodeReset acknowledges uncertainty; the caller must explicitly
// warn that the vendor may already have spent a reset before resolving it.
func ResolveZCodeReset(ctx context.Context, providerID, account string) error {
	expectation, ok := expectedZCodeAccount(ctx)
	if !ok || expectation.scope == "" || expectation.generation == "" {
		return errors.New("capture the ZCode account scope and generation before resolving a reset")
	}
	key, err := zcodeAccountKey(providerID, account)
	if err != nil {
		return err
	}
	c, err := zcodeContext(ctx, providerID, key)
	if err != nil {
		return err
	}
	return resolveZCodeReset(c)
}

func resolveZCodeReset(c plugin.AccountContext) error {
	unlock, err := zcodeFence(c, "reset")
	if err != nil {
		return err
	}
	defer unlock()
	return withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		if j.Reset.Pending == nil {
			return false, errors.New("there is no unconfirmed reset to resolve")
		}
		j.Reset.Pending = nil
		return true, nil
	})
}
