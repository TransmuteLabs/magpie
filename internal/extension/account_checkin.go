package extension

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) CheckinAccount(ctx context.Context, scope sdk.Scope, revision, config string, request sdk.AccountCheckinRequest) (CheckinExecution, error) {
	return m.CheckinAccountWithAdmission(ctx, scope, revision, config, request, nil)
}

type checkinAdmissionKey struct{}

type checkinAdmission struct {
	scope sdk.Scope
	check func(context.Context) error
}

func (m *Manager) CheckinAccountWithAdmission(ctx context.Context, scope sdk.Scope, revision, config string, request sdk.AccountCheckinRequest,
	admit func(context.Context) error,
) (CheckinExecution, error) {
	if request.Validate() != nil || scope.Account == "" || scope.Generation == 0 ||
		scope.Operation != "" || scope.Attempt != 0 || scope.Session != "" ||
		revision == "" || config == "" {
		return CheckinExecution{}, errors.New("check-in requires its selected account and exact package/configuration")
	}
	if err := netproxy.Held(ctx); err != nil {
		return CheckinExecution{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if admit != nil {
		if err := admit(ctx); err != nil {
			return CheckinExecution{}, err
		}
	}
	lease, err := m.AcquireRevision(ctx, scope.Plugin, revision, config)
	if err != nil {
		return CheckinExecution{}, err
	}
	defer lease.Close()
	manifest := lease.worker.entry.Manifest
	descriptor, found := manifest.AccountCheckin(scope.Provider)
	if !found || manifest.Validate() != nil {
		return CheckinExecution{}, errors.New("leased account check-in was not negotiated")
	}
	if err := lease.CheckCurrent(ctx); err != nil {
		return CheckinExecution{}, err
	}
	if _, err := m.authForContext(ctx, scope); err != nil {
		return CheckinExecution{}, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return CheckinExecution{}, err
	}
	if admit != nil {
		if err := admit(ctx); err != nil {
			return CheckinExecution{}, err
		}
	}
	record, dispatch, err := m.store.reserveCheckin(ctx, scope, request.Day)
	if err != nil || !dispatch {
		return checkinExecution(record), err
	}
	scope = record.Receipt.Scope
	ctx = context.WithValue(ctx, checkinAdmissionKey{}, checkinAdmission{scope: scope, check: admit})
	input, _ := json.Marshal(request)
	var raw json.RawMessage
	if err = lease.CheckCurrent(ctx); err == nil {
		raw, err = lease.Call(ctx, sdk.MethodModuleCall, scope, sdk.ModuleCallRequest{
			Action: descriptor.Action, Input: input,
		}, func(event Event) error {
			if event.Name == "accepted" && manifest.Contract.Has(sdk.UsageReporter) {
				_, invalid := m.store.InvalidateUsage(scope)
				return invalid
			}
			return nil
		})
	}
	receipt, journalErr := m.store.Operation(scope.Plugin, scope.Operation)
	if journalErr == nil {
		record.Receipt = receipt
	} else {
		var missing *sdk.Error
		if errors.As(journalErr, &missing) && missing.Code == "not_found" {
			record.Receipt.State = "not-started"
		} else {
			record.Receipt.State = "uncertain"
		}
		record.Receipt.Updated = time.Now().UTC()
	}
	if err == nil {
		var result sdk.AccountCheckinResult
		result, err = sdk.DecodeAccountCheckinResult(raw)
		if err == nil && result.Outcome == sdk.AccountCheckinRateLimited {
			if manifest.Contract.Version.Minor < sdk.AccountCheckinRateLimitAPIMinor {
				err = errors.New("rate-limited check-in result requires SDK API 1.20")
			} else if record.Receipt.State != "completed" {
				err = errors.New("rate-limited check-in result requires a completed operation receipt")
			}
		}
		if err == nil {
			record.Result = &result
		} else {
			record.Receipt.State = "uncertain"
		}
	}
	if stored := m.store.finishCheckin(record); stored != nil {
		return checkinExecution(record), &OutcomeError{Outcome: "uncertain", Cause: stored}
	}
	return checkinExecution(record), err
}

func checkLeasedCheckin(ctx context.Context, lease *Lease, method string, scope sdk.Scope, params any) error {
	if method != sdk.MethodModuleCall {
		return nil
	}
	raw, err := json.Marshal(params)
	var action sdk.ModuleCallRequest
	if err != nil || !decodeParamsLimit(raw, &action, sdk.MaxFrameBytes-4096) {
		return errors.New("invalid module action")
	}
	manifest := lease.worker.entry.Manifest
	checkinAction := false
	for _, provider := range manifest.Providers {
		checkinAction = checkinAction || provider.Checkin != nil && provider.Checkin.Action == action.Action
	}
	if !checkinAction {
		return nil
	}
	descriptor, found := manifest.AccountCheckin(scope.Provider)
	var request sdk.AccountCheckinRequest
	admission, owned := ctx.Value(checkinAdmissionKey{}).(checkinAdmission)
	if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(sdk.AccountAuth) ||
		!manifest.Contract.Has(sdk.ModuleRPC) || !found || descriptor.Action != action.Action ||
		scope.Account == "" || scope.Generation == 0 || scope.Session != "" ||
		!owned || admission.scope != scope ||
		!decodeHostParams(action.Input, &request) || request.Validate() != nil ||
		request.Proxy != netproxy.Choice(ctx) {
		return &OutcomeError{Outcome: "not-started", Cause: errors.New("leased check-in requires its reserved selected-account intent")}
	}
	if err := lease.CheckCurrent(ctx); err != nil {
		return &OutcomeError{Outcome: "not-started", Cause: err}
	}
	if err := netproxy.Held(ctx); err != nil {
		return &OutcomeError{Outcome: "not-started", Cause: err}
	}
	if admission.check != nil {
		if err := admission.check(ctx); err != nil {
			return &OutcomeError{Outcome: "not-started", Cause: err}
		}
	}
	return nil
}
