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
	if _, err := m.authFor(scope); err != nil {
		return CheckinExecution{}, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return CheckinExecution{}, err
	}
	record, dispatch, err := m.store.reserveCheckin(scope, request.Day)
	if err != nil || !dispatch {
		return checkinExecution(record), err
	}
	scope = record.Receipt.Scope
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
	if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(sdk.AccountAuth) ||
		!manifest.Contract.Has(sdk.ModuleRPC) || !found || descriptor.Action != action.Action ||
		scope.Account == "" || scope.Generation == 0 || scope.Session != "" ||
		!decodeHostParams(action.Input, &request) || request.Validate() != nil ||
		request.Proxy != netproxy.Choice(ctx) {
		return &OutcomeError{Outcome: "not-started", Cause: errors.New("leased check-in requires its negotiated selected-account input")}
	}
	if err := lease.CheckCurrent(ctx); err != nil {
		return &OutcomeError{Outcome: "not-started", Cause: err}
	}
	if err := netproxy.Held(ctx); err != nil {
		return &OutcomeError{Outcome: "not-started", Cause: err}
	}
	return nil
}
