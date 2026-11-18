package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type credentials struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Mode  string `json:"mode"`
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	if len(os.Args) > 1 {
		manifest, err = sdk.ServiceManifest(manifest, os.Args[1])
		if err != nil {
			panic(err)
		}
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{}, Actions: map[string]sdk.Handler{}}
	var taskStarted atomic.Bool
	if manifest.Component == nil {
		for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh, sdk.MethodAuthLogout} {
			module.Handlers[method] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				return sdk.AuthResult{ID: call.Scope.Account, Credentials: call.Auth, ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}, nil
			}
		}
		module.Handlers[sdk.MethodProviderUsage] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.UsageRequest
			_ = json.Unmarshal(call.Params, &request)
			var auth credentials
			_ = json.Unmarshal(call.Auth, &auth)
			if auth.Mode == "hold-usage-deferred" && taskStarted.Load() {
				return sdk.UsageResult{Revision: request.Revision, ReadDeferred: true}, nil
			}
			window := sdk.UsageWindow{Name: "quota", Used: 10}
			if auth.Mode == "quota-exhausted" {
				window.Used = 100
			}
			if auth.Mode == "quota-unknown" {
				window.Unknown = true
			}
			return sdk.UsageResult{Revision: request.Revision, Windows: []sdk.UsageWindow{window}}, nil
		}
		module.Handlers[sdk.MethodProviderExecute] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var execute sdk.ExecuteRequest
			_ = json.Unmarshal(call.Params, &execute)
			key := "conversation"
			if bytes.Contains(execute.Body, []byte(":other")) {
				key = "other-conversation"
			}
			if _, err := stream.CancelAccountTask(ctx, sdk.AccountTaskCancelRequest{Task: "ping", Key: key}); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			content := "foreground"
			if bytes.Contains(execute.Body, []byte("schedule")) {
				dueAt := time.Now().Add(time.Second).UnixMilli()
				var auth credentials
				_ = json.Unmarshal(call.Auth, &auth)
				if auth.Mode == "isolation" {
					dueAt = time.Now().Add(30 * time.Minute).UnixMilli()
				}
				if strings.Contains(string(execute.Body), ":overdue") {
					dueAt = time.Now().Add(-time.Second).UnixMilli()
				}
				task, err := stream.ScheduleAccountTask(ctx, sdk.AccountTaskScheduleRequest{Task: "ping", Key: key,
					Job: "opaque-fixture-template", DueAt: dueAt})
				if err != nil {
					return nil, err
				}
				content = task.OperationID
			}
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(map[string]any{"id": "foreground", "choices": []any{
				map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
			if err := stream.Emit(ctx, "data", raw); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Status: 200, Outcome: "completed"}, nil
		}
	}
	paid := func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		var auth credentials
		if json.Unmarshal(call.Auth, &auth) != nil {
			return nil, errors.New("bad fixture authority")
		}
		task, err := sdk.DecodeAccountTaskRequest(call.Params)
		if err != nil {
			return nil, err
		}
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
		taskStarted.Store(true)
		body, _ := json.Marshal(map[string]any{"scope": call.Scope, "task": task, "token": auth.Token})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL+"/paid", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		var proxy sdk.NetworkProxyResult
		if err := stream.HostCall(ctx, sdk.MethodNetworkProxy, sdk.NetworkProxyRequest{URL: auth.URL}, &proxy); err != nil {
			return nil, err
		}
		transport := &http.Transport{}
		defer transport.CloseIdleConnections()
		if proxy.URL != "" {
			selected, err := url.Parse(proxy.URL)
			if err != nil {
				return nil, err
			}
			transport.Proxy = http.ProxyURL(selected)
		}
		client := &http.Client{Transport: transport}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		if auth.Mode == "snapshots" {
			defer response.Body.Close()
			decoder := json.NewDecoder(response.Body)
			for range 2 {
				var own sdk.OwnCall
				if err := decoder.Decode(&own); err != nil {
					return nil, err
				}
				if err := stream.ReportAccountTaskUsage(ctx, own); err != nil {
					return nil, err
				}
			}
			scope, _ := json.Marshal(call.Scope)
			ready, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL+"/snapshots-ready", bytes.NewReader(scope))
			if err != nil {
				return nil, err
			}
			acknowledged, err := client.Do(ready)
			if err != nil {
				return nil, err
			}
			if err := acknowledged.Body.Close(); err != nil {
				return nil, err
			}
			var next sdk.OwnCall
			if err := decoder.Decode(&next); err != nil {
				return nil, err
			}
			return nil, errors.New("fixture expected cancellation before another usage snapshot")
		}
		raw, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != 200 {
			return nil, errors.New("paid fixture failed after acceptance")
		}
		var own sdk.OwnCall
		if json.Unmarshal(raw, &own) != nil {
			return nil, errors.New("bad fixture usage")
		}
		if auth.Mode == "duplicate" {
			if _, err := stream.AppendOwnCall(ctx, own); err != nil {
				return nil, err
			}
		}
		if auth.Mode == "uncertain" {
			return sdk.AccountTaskResult{Outcome: "uncertain", Call: &own}, nil
		}
		if auth.Mode == "missing-usage" {
			return sdk.AccountTaskResult{Outcome: "completed"}, nil
		}
		if auth.Mode == "chain" && task.Job == "opaque-fixture-template" {
			if err := stream.ReportAccountTaskUsage(ctx, own); err != nil {
				return nil, err
			}
			if _, err := stream.ScheduleAccountTask(ctx, sdk.AccountTaskScheduleRequest{
				Task: task.Task, Key: task.Key, Job: "opaque-fixture-template-next", DueAt: time.Now().Add(-time.Second).UnixMilli(),
			}); err != nil {
				return nil, err
			}
		}
		return sdk.AccountTaskResult{Outcome: "completed", Call: &own}, nil
	}
	action, declared := manifest.ResolveAction("paid.ping")
	owner := action.Component
	if owner == "" {
		owner = sdk.PrimaryComponent
	}
	component := sdk.PrimaryComponent
	if manifest.Component != nil {
		component = manifest.Component.ID
	}
	if declared && owner == component {
		module.Actions[action.ID] = paid
	}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
