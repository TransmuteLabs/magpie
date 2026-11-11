package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

type zcodeBillingClaim struct {
	PlanID        string          `json:"planId"`
	Claim         string          `json:"claim"`
	Generation    string          `json:"generation"`
	State         string          `json:"state"`
	BaselineKnown bool            `json:"baselineKnown"`
	Baseline      []string        `json:"baseline,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	EndsAt        float64         `json:"endsAt,omitempty"`
}

func validZCodeBillingClaims(claims map[string]zcodeBillingClaim) bool {
	for id, claim := range claims {
		if id == "" || len(id) > 128 || id != claim.PlanID || claim.Claim == "" || claim.Generation == "" ||
			(claim.State != "pending" && claim.State != "confirmed" && claim.State != "observed") ||
			claim.EndsAt < 0 || len(claim.Result) > 0 && !json.Valid(claim.Result) {
			return false
		}
		for _, entry := range claim.Baseline {
			if entry == "" {
				return false
			}
		}
	}
	return true
}

// Billing is a ZCode-user entitlement, not a Coding Plan selection. This
// journal key and fence are shared by that user's Personal, Team and Start
// slots, including after a new login generation.
func zcodeBillingContext(c plugin.AccountContext) (plugin.AccountContext, error) {
	if strings.TrimSpace(c.UserID) == "" {
		return c, errors.New("ZCode billing identity is unavailable; sign in with an authoritative user identity")
	}
	sum := sha256.Sum256([]byte(c.Origin + "\x00" + c.UserID))
	c.Scope = "billing:" + hex.EncodeToString(sum[:])
	c.Kind = "billing"
	return c, nil
}

type zcodeBillingPlan struct {
	PlanID     string  `json:"plan_id"`
	UserPlanID string  `json:"user_plan_id"`
	Status     string  `json:"status"`
	EndsAt     float64 `json:"ends_at"`
}

func zcodeBillingPlans(ctx context.Context, providerID, account string, c plugin.AccountContext) ([]zcodeBillingPlan, error) {
	data, err := zcodeAccountCall(ctx, providerID, account, c, "billing.status", nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Plans []zcodeBillingPlan `json:"plans"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Plans == nil {
		return nil, errors.New("ZCode billing status is unavailable")
	}
	for _, p := range response.Plans {
		if p.PlanID == "" || p.UserPlanID == "" || p.Status == "" || p.EndsAt < 0 {
			return nil, errors.New("ZCode billing status is incomplete")
		}
	}
	return response.Plans, nil
}

func zcodePlanIDs(plans []zcodeBillingPlan, id string) []string {
	var out []string
	for _, p := range plans {
		if p.PlanID == id && !slices.Contains(out, p.UserPlanID) {
			out = append(out, p.UserPlanID)
		}
	}
	return out
}

func zcodeClaim(ctx context.Context, providerID, account string, c plugin.AccountContext, params any) (json.RawMessage, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var input struct {
		PlanID string `json:"plan_id"`
	}
	if json.Unmarshal(b, &input) != nil || strings.TrimSpace(input.PlanID) != input.PlanID || input.PlanID == "" || len(input.PlanID) > 128 {
		return nil, errors.New("billing.claim requires a plan_id")
	}
	owner, err := zcodeBillingContext(c)
	if err != nil {
		return nil, err
	}
	unlock, err := zcodeFence(owner, "claim")
	if err != nil {
		return nil, err
	}
	defer unlock()
	var existing zcodeBillingClaim
	if err := withZCodeJournal(owner, func(j *zcodeJournal) (bool, error) {
		existing = j.Claims[input.PlanID]
		return false, nil
	}); err != nil {
		return nil, err
	}
	if existing.State == "pending" {
		if existing.BaselineKnown {
			if plans, readErr := zcodeBillingPlans(ctx, providerID, account, c); readErr == nil {
				for _, plan := range plans {
					if plan.PlanID != input.PlanID || slices.Contains(existing.Baseline, plan.UserPlanID) {
						continue
					}
					observed, _ := json.Marshal(map[string]any{"outcome": "observed", "plan": plan})
					err = withZCodeJournal(owner, func(j *zcodeJournal) (bool, error) {
						claim := j.Claims[input.PlanID]
						if claim.Claim != existing.Claim || claim.State != "pending" {
							return false, errors.New("billing claim changed during reconciliation")
						}
						claim.State, claim.Result, claim.EndsAt = "observed", observed, plan.EndsAt
						j.Claims[input.PlanID] = claim
						return true, nil
					})
					return observed, err
				}
			}
		}
		return nil, &plugin.AccountError{Kind: "claim_unconfirmed", Message: "a previous claim may have succeeded; its outcome remains unconfirmed", Dispatched: true, DispatchedKnown: true}
	}
	// An entitlement appearing after a lost reply does not prove this
	// claim caused it. Even after that entitlement expires, automatically
	// posting again would replay an operation whose outcome is unknown.
	if existing.State == "observed" {
		return existing.Result, nil
	}
	if existing.State == "confirmed" {
		expired := existing.EndsAt > 0 && existing.EndsAt*1000 <= float64(time.Now().UnixMilli())
		if !expired && existing.EndsAt <= 0 {
			plans, readErr := zcodeBillingPlans(ctx, providerID, account, c)
			found := false
			if readErr == nil {
				expired = true
				for _, plan := range plans {
					if plan.PlanID != input.PlanID {
						continue
					}
					found = true
					if !strings.EqualFold(plan.Status, "expired") {
						expired = false
					}
				}
				expired = expired && found
			}
		}
		if !expired {
			return existing.Result, nil
		}
	}
	baseline, baselineErr := zcodeBillingPlans(ctx, providerID, account, c)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if ZCodeAccountChanged(baselineErr) {
		return nil, baselineErr
	}
	claimID, err := zcodeJournalKey()
	if err != nil {
		return nil, err
	}
	claim := zcodeBillingClaim{PlanID: input.PlanID, Claim: claimID, Generation: c.Generation, State: "pending", BaselineKnown: baselineErr == nil}
	if baselineErr == nil {
		claim.Baseline = zcodePlanIDs(baseline, input.PlanID)
	}
	if err := withZCodeJournal(owner, func(j *zcodeJournal) (bool, error) {
		if j.Claims == nil {
			j.Claims = map[string]zcodeBillingClaim{}
		}
		if current := j.Claims[input.PlanID]; current.State == "pending" {
			return false, errors.New("a ZCode billing claim is already unconfirmed")
		}
		j.Claims[input.PlanID] = claim
		return true, nil
	}); err != nil {
		return nil, err
	}
	data, callErr := zcodeAccountCall(ctx, providerID, account, c, "billing.claim.raw", map[string]string{"plan_id": input.PlanID})
	var accountErr *plugin.AccountError
	definiteNoDispatch := errors.As(callErr, &accountErr) && accountErr.DispatchedKnown && !accountErr.Dispatched
	already := errors.As(callErr, &accountErr) && accountErr.Kind == "business" && accountErr.Code == 1003
	definiteRefusal := errors.As(callErr, &accountErr) && accountErr.Kind == "business" && (accountErr.Code == 1004 || accountErr.Code == 3007)
	if already {
		data, _ = json.Marshal(map[string]any{"outcome": "already-claimed", "plan_id": input.PlanID})
	}
	var result struct {
		Plan struct {
			PlanID string  `json:"plan_id"`
			EndsAt float64 `json:"ends_at"`
		} `json:"plan"`
	}
	if callErr == nil && (json.Unmarshal(data, &result) != nil || result.Plan.PlanID != input.PlanID) {
		callErr = errors.New("billing claim returned an unreadable outcome")
	}
	finishErr := withZCodeJournal(owner, func(j *zcodeJournal) (bool, error) {
		current := j.Claims[input.PlanID]
		if current.Claim != claimID || current.State != "pending" {
			return false, errors.New("billing claim changed while in flight")
		}
		if definiteNoDispatch || definiteRefusal {
			delete(j.Claims, input.PlanID)
			return true, nil
		}
		if callErr == nil || already {
			current.State, current.Result, current.EndsAt = "confirmed", data, result.Plan.EndsAt
			j.Claims[input.PlanID] = current
		}
		return true, nil
	})
	if finishErr != nil {
		return nil, fmt.Errorf("billing claim outcome requires reconciliation: %w", finishErr)
	}
	if already {
		return data, nil
	}
	if callErr != nil {
		if definiteNoDispatch || definiteRefusal {
			return nil, callErr
		}
		return nil, &plugin.AccountError{Kind: "claim_unconfirmed", Message: "claim outcome is unknown; it may already have succeeded", Dispatched: true, DispatchedKnown: true}
	}
	return data, nil
}

func zcodeScheduledCheckin(ctx context.Context, providerID, account string) WorkBuddyCheckin {
	c, err := zcodeContext(ctx, providerID, account)
	if err != nil {
		return wbCheckinFailed(err)
	}
	data, err := zcodeAccountCall(ctx, providerID, account, c, "marketing.feed", map[string]any{})
	if err != nil {
		return wbCheckinFailed(err)
	}
	var feed struct {
		Rejected   int `json:"rejectedCount"`
		Deliveries []struct {
			Banner struct {
				Buttons []struct {
					Action struct {
						Type string `json:"type"`
						Args struct {
							PlanID string `json:"plan_id"`
						} `json:"args"`
					} `json:"action"`
				} `json:"buttons"`
			} `json:"banner"`
			Popup struct {
				Buttons []struct {
					Action struct {
						Type string `json:"type"`
						Args struct {
							PlanID string `json:"plan_id"`
						} `json:"args"`
					} `json:"action"`
				} `json:"buttons"`
			} `json:"popup"`
		} `json:"deliveries"`
	}
	if json.Unmarshal(data, &feed) != nil || feed.Deliveries == nil {
		return wbCheckinFailed(errors.New("ZCode marketing feed is unreadable"))
	}
	var plans []string
	add := func(kind, id string) {
		if kind == "claim_zcode_plan" && id != "" && !slices.Contains(plans, id) {
			plans = append(plans, id)
		}
	}
	for _, d := range feed.Deliveries {
		for _, button := range d.Banner.Buttons {
			add(button.Action.Type, button.Action.Args.PlanID)
		}
		for _, button := range d.Popup.Buttons {
			add(button.Action.Type, button.Action.Args.PlanID)
		}
	}
	if feed.Rejected > 0 {
		return wbCheckinFailed(errors.New("part of the ZCode marketing feed could not be interpreted"))
	}
	if len(plans) == 0 {
		return WorkBuddyCheckin{Outcome: CheckinInactive, Msg: "no ZCode gift plan to claim", ValidUntil: time.Now().Add(30 * time.Minute)}
	}
	claimed := false
	for _, id := range plans {
		result, err := zcodeClaim(ctx, providerID, account, c, map[string]string{"plan_id": id})
		if err != nil {
			return wbCheckinFailed(err)
		}
		var outcome struct {
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(result, &outcome)
		if outcome.Outcome == "observed" {
			return wbCheckinFailed(errors.New("a ZCode entitlement appeared after an unconfirmed claim; its cause is uncertain"))
		}
		claimed = claimed || outcome.Outcome == ""
	}
	if claimed {
		return WorkBuddyCheckin{Outcome: CheckinClaimed, Msg: "ZCode gift plan claimed"}
	}
	return WorkBuddyCheckin{Outcome: CheckinDone, Msg: "ZCode gift plan already claimed or observed"}
}
