package gateway

import (
	"context"
	"net/http"
	"slices"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwmw"
	"github.com/yetone/magpie/internal/middleware"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionMiddlewareAgent struct {
	selection extension.GatewaySelection
	ctx       context.Context
}

type extensionMiddlewareCall struct {
	run        *extension.MiddlewareRun
	descriptor sdk.GatewayMiddlewareDescriptor
	provider   *sdk.MiddlewareProvider
	account    *sdk.MiddlewareAccount
}

type extensionMiddlewareRun struct {
	calls []extensionMiddlewareCall
}

func (agent extensionMiddlewareAgent) Begin(info gwmw.Info) gwmw.Run {
	runs := agent.selection.BeginMiddleware(agent.ctx, sdk.GatewayContext{
		Protocol: info.Protocol, Model: info.Model, Stream: info.Stream, Path: info.Path, Agent: info.Agent})
	if len(runs) == 0 {
		return nil
	}
	run := &extensionMiddlewareRun{}
	for _, current := range runs {
		run.calls = append(run.calls, extensionMiddlewareCall{run: current, descriptor: current.Descriptor()})
	}
	return run
}

func (run *extensionMiddlewareRun) Request(body []byte) ([]byte, *gwmw.Reject) {
	for _, call := range run.calls {
		if !call.descriptor.Request {
			continue
		}
		result, err := call.run.Apply(sdk.MethodMiddlewareRequest, body, sdk.MiddlewareContext{})
		if err != nil {
			continue
		}
		if result.Reject != nil {
			return body, &gwmw.Reject{Status: result.Reject.Status, Message: result.Reject.Message}
		}
		if len(result.Body) > 0 {
			body = result.Body
		}
	}
	return body, nil
}

func (run *extensionMiddlewareRun) Account(account gwmw.Account) (bool, string) {
	for index := range run.calls {
		call := &run.calls[index]
		call.account, call.provider = nil, nil
		if account.Agent == "" {
			continue
		}
		call.account = &sdk.MiddlewareAccount{Ref: middleware.AccountReference(call.run.Module, account),
			Provider: account.ID, Name: account.Name, Kind: account.Agent, Model: account.Model}
		if !call.descriptor.Account {
			continue
		}
		result, err := call.run.Apply(sdk.MethodMiddlewareAccount, nil, sdk.MiddlewareContext{Account: call.account})
		if err == nil && result.Skip {
			return true, call.run.Module
		}
	}
	return false, ""
}

func (run *extensionMiddlewareRun) Tried(body []byte, to gwmw.To) []byte {
	for index := range run.calls {
		call := &run.calls[index]
		call.provider = &sdk.MiddlewareProvider{ID: to.ID, Name: to.Name, Account: to.Account, Model: to.Model}
		if !call.descriptor.Provider {
			continue
		}
		result, err := call.run.Apply(sdk.MethodMiddlewareProvider, body,
			sdk.MiddlewareContext{Provider: call.provider, Account: call.account})
		if err == nil && len(result.Body) > 0 {
			body = result.Body
		}
	}
	return body
}

func (run *extensionMiddlewareRun) Wrap(w http.ResponseWriter) (http.ResponseWriter, func()) {
	hooks := middleware.ResponseHooks{MaxBytes: sdk.MaxMiddlewareBodyBytes}
	if slices.ContainsFunc(run.calls, func(call extensionMiddlewareCall) bool { return call.descriptor.Event }) {
		hooks.Event = func(name string, body []byte) ([]byte, bool) {
			changed := false
			for _, call := range run.calls {
				descriptor := call.descriptor
				if !descriptor.Event || len(descriptor.Events) > 0 && !slices.Contains(descriptor.Events, name) {
					continue
				}
				result, err := call.run.Apply(sdk.MethodMiddlewareEvent, body,
					sdk.MiddlewareContext{Provider: call.provider, Account: call.account, Event: name})
				if err != nil {
					continue
				}
				if result.Drop {
					return nil, true
				}
				if len(result.Body) > 0 {
					body, changed = result.Body, true
				}
			}
			if changed {
				return body, false
			}
			return nil, false
		}
	}
	if slices.ContainsFunc(run.calls, func(call extensionMiddlewareCall) bool { return call.descriptor.Response }) {
		hooks.Response = func(body []byte, status int) []byte {
			for _, call := range run.calls {
				if !call.descriptor.Response {
					continue
				}
				result, err := call.run.Apply(sdk.MethodMiddlewareResponse, body,
					sdk.MiddlewareContext{Provider: call.provider, Account: call.account, Status: status})
				if err == nil && len(result.Body) > 0 {
					body = result.Body
				}
			}
			return body
		}
	}
	return middleware.WrapResponse(w, hooks)
}

func (run *extensionMiddlewareRun) End() {
	for _, call := range run.calls {
		call.run.Close()
	}
}
