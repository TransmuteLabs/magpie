package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/extension/native"
)

func module(kind string) sdk.Module {
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(`{"schema":1,"id":"fixture.decision","version":"1.0.0",
		"runtime":"exec","entry":"worker",
		"compatibility":{"hostAPI":"^1.7"},
		"contract":{"version":{"major":1,"minor":7},"capabilities":["outbound-executor","model-discovery","account-auth"]},
		"providers":[{"id":"judge","name":"Judge","protocols":["chat"],"operations":["systemone"],
		"models":[{"id":"decision","kind":"decision","operations":["systemone"]},{"id":"conversation","kind":"chat"}]}]}`),
		&manifest); err != nil {
		panic(err)
	}
	manifest.Runtime = kind
	if kind == "native" {
		manifest.Entry = "worker.so"
		if runtime.GOOS == "darwin" {
			manifest.Entry = "worker.dylib"
		} else if runtime.GOOS == "windows" {
			manifest.Entry = "worker.dll"
		}
	} else if runtime.GOOS == "windows" {
		manifest.Entry = "worker.exe"
	}
	unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, &sdk.Error{Code: "unsupported", Message: "fixture auth is preprovisioned", Outcome: "not-started"}
	}
	return sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: execute, sdk.MethodProviderModels: models,
		sdk.MethodAuthStart: unsupported, sdk.MethodAuthFinish: unsupported,
		sdk.MethodAuthRefresh: unsupported, sdk.MethodAuthLogout: unsupported,
	}}
}

func init() {
	if err := native.Register(module("native")); err != nil && !errors.Is(err, native.ErrUnavailable) {
		panic(err)
	}
}

func models(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
	var auth struct{ Mode string }
	_ = json.Unmarshal(call.Auth, &auth)
	if auth.Mode == "empty" {
		return sdk.ModelsResult{Models: []sdk.Model{}}, nil
	}
	return sdk.ModelsResult{Models: module("exec").Manifest.Providers[0].Models}, nil
}

func record(ctx context.Context, call sdk.Call, stream *sdk.Stream, request sdk.ExecuteRequest) error {
	var state sdk.StateValue
	if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "executions"}, &state); err != nil {
		return err
	}
	var entries []any
	if len(state.Value) > 0 {
		if err := json.Unmarshal(state.Value, &entries); err != nil {
			return err
		}
	}
	entries = append(entries, map[string]any{"operation": call.Scope.Operation, "account": call.Scope.Account,
		"request": request})
	raw, _ := json.Marshal(entries)
	var result sdk.StateCASResult
	if err := stream.HostCall(ctx, sdk.MethodStateCAS,
		sdk.StateCASRequest{Key: "executions", Version: state.Version, Value: raw}, &result); err != nil {
		return err
	}
	if !result.Swapped {
		return errors.New("fixture execution trace conflicted")
	}
	return nil
}

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ExecuteRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if err := record(ctx, call, stream, request); err != nil {
		return nil, err
	}
	if request.Protocol != "" || request.Operation != "systemone" || request.Model != "decision" ||
		request.Headers.Get("Authorization") != "" || request.Headers.Get("Cookie") != "" || len(request.Credentials) > 0 {
		return nil, &sdk.Error{Code: "invalid_scope", Message: "decision fixture saw the wrong envelope or caller authority", Outcome: "not-started"}
	}
	var auth struct{ Mode string }
	_ = json.Unmarshal(call.Auth, &auth)
	calibration := strings.Contains(string(request.Body), `"levels":{"instructions"`) ||
		strings.Contains(string(request.Body), `"Are these `+"`kinds`")
	if auth.Mode == "not-started" || auth.Mode == "main not-started" && !calibration {
		return nil, &sdk.Error{Code: "not_started", Message: "fixture did not start", Outcome: "not-started"}
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	totals := &sdk.Usage{InputTokens: 41, OutputTokens: 3, CachedTokens: 5, CacheWriteTokens: 2}
	if calibration {
		totals = &sdk.Usage{InputTokens: 11, OutputTokens: 2}
	}
	if strings.HasPrefix(auth.Mode, "rich") && !calibration {
		totals = &sdk.Usage{}
		if err := json.Unmarshal([]byte(`{"facts":{"inputTokens":0,"cachedTokens":7,"cacheWriteTokens":5,"cacheWrite1hTokens":2,
			"reasoningTokens":2,"deferred":{"inputTokens":4,"outputTokens":1,"cacheWriteTokens":2,"cacheWrite1hTokens":1,"reasoningTokens":1},
			"servedModel":"served-decision","upstreamProvider":"fixture-upstream","requestId":"upstream-request","responseId":"decision-response",
			"stop":"answer","tier":"priority","credits":0.125,"cost":{"amount":0.05,"currency":"USD"},"errorType":"fixture_terminal"}}`),
			&totals); err != nil {
			return nil, err
		}
		if auth.Mode == "rich nonUSD" {
			totals.Facts.Cost.Currency = "CNY"
		}
	}
	if auth.Mode == "main nil usage" && !calibration {
		totals = nil
	}
	if totals != nil {
		event := totals.Clone()
		if event.Facts != nil {
			inflated := int64(999)
			event.Facts.InputTokens, event.Facts.OutputTokens = &inflated, &inflated
		} else {
			event.InputTokens--
		}
		if err := stream.Emit(ctx, "usage", event); err != nil {
			return nil, err
		}
	}
	if auth.Mode == "accepted error" {
		return nil, &sdk.Error{Code: "fixture_failed", Message: "fixture failed after acceptance", Outcome: "accepted"}
	}
	if auth.Mode == "deadline" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	status, outcome := http.StatusOK, "completed"
	if auth.Mode == "accepted 503" || auth.Mode == "rich failure" {
		status = http.StatusServiceUnavailable
	}
	header := http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {call.Scope.Operation},
		"Retry-After": {"7"}, "Set-Cookie": {"private=secret"}, "Connection": {"keep-alive"},
		"X-Should-Retry": {"true"}, "X-Magpie-Operation": {"forged"},
		"X-Fixture-Account": {call.Scope.Account}}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: status, Headers: header}); err != nil {
		return nil, err
	}
	answer := []byte(`{"model":"decision","answers":{"levels":{"type":"noul","noul":0},"intent":{"type":"choice","choice":"bug","confidence":0.99},"safe":{"type":"noul","noul":0.93}},"usage":{"input_tokens":41,"output_tokens":3}}`)
	if auth.Mode == "invalid answer" {
		answer = []byte(`{"model":"decision","answers":{}}`)
	}
	if auth.Mode == "malformed answer" {
		answer = []byte(`{"model":"decision","answers":`)
	}
	if status >= 400 {
		answer = []byte(`{"error":{"message":"accepted decision failed"},"usage":{"input_tokens":41,"output_tokens":3}}`)
	}
	if err := stream.Emit(ctx, "data", answer); err != nil {
		return nil, err
	}
	if auth.Mode == "partial" {
		return nil, &sdk.Error{Code: "cut", Message: "decision response cut", Outcome: "uncertain"}
	}
	if auth.Mode == "event only" || auth.Mode == "rich event only" {
		totals = nil
	}
	if auth.Mode == "rich empty final" {
		totals = &sdk.Usage{Facts: &sdk.ExecutionUsageFacts{}}
	}
	if auth.Mode == "zero result" {
		totals = &sdk.Usage{}
	}
	if auth.Mode == "uncertain" {
		outcome = "uncertain"
	}
	return sdk.ExecuteResult{Status: status, Outcome: outcome, Usage: totals}, nil
}

func main() {
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module("exec")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
