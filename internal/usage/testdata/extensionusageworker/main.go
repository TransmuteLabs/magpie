package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type reportRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Accept bool            `json:"accept"`
	Repeat int             `json:"repeat"`
	Wait   string          `json:"wait"`
}

func waitForRelease(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("fixture barrier response failed: %v %v", readErr, closeErr)
	}
	return nil
}

func report(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request reportRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if request.Accept {
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
	}
	if err := waitForRelease(ctx, request.Wait); err != nil {
		return nil, err
	}
	if request.Repeat == 0 {
		request.Repeat = 1
	}
	var results []json.RawMessage
	for range request.Repeat {
		var result json.RawMessage
		if err := stream.HostCall(ctx, request.Method, request.Params, &result); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
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
	module := sdk.Module{
		Manifest: manifest,
		Actions: map[string]sdk.Handler{
			"usagefixture.own": report, "usagefixture.history": report,
		},
		Handlers: map[string]sdk.Handler{
			sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				var request sdk.ExecuteRequest
				if err := json.Unmarshal(call.Params, &request); err != nil {
					return nil, err
				}
				call.Params = request.Body
				_, err := report(ctx, call, stream)
				if err == nil {
					return nil, fmt.Errorf("inference acquired separate usage-report authority")
				}
				return sdk.ExecuteResult{Outcome: "completed", Status: 200}, nil
			},
			sdk.MethodProviderUsage: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.UsageResult{}, nil
			},
		},
	}
	if slices.Contains(sdk.RequiredMethods(manifest), "provider.calls") {
		module.Handlers["provider.calls"] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request struct {
				Since  int64  `json:"since"`
				Until  int64  `json:"until"`
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			var state sdk.StateValue
			if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "calls-fixture"}, &state); err != nil {
				return nil, err
			}
			var fixture struct {
				Since *int64                     `json:"since"`
				Pages map[string]json.RawMessage `json:"pages"`
				Wait  string                     `json:"wait"`
				Event string                     `json:"event"`
			}
			if err := json.Unmarshal(state.Value, &fixture); err != nil {
				return nil, err
			}
			if fixture.Since != nil && *fixture.Since != request.Since {
				return nil, fmt.Errorf("history watermark changed after failure: got %d want %d", request.Since, *fixture.Since)
			}
			if err := waitForRelease(ctx, fixture.Wait); err != nil {
				return nil, err
			}
			if fixture.Event != "" {
				if err := stream.Emit(ctx, fixture.Event, sdk.Usage{InputTokens: 1}); err != nil {
					return nil, err
				}
			}
			page, exists := fixture.Pages[request.Cursor]
			if !exists {
				return nil, fmt.Errorf("missing history cursor %q", request.Cursor)
			}
			var failure struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(page, &failure); err != nil || failure.Error != "" {
				return nil, fmt.Errorf("history fixture page failed: %s", failure.Error)
			}
			return page, nil
		}
	}
	for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh, sdk.MethodAuthLogout} {
		module.Handlers[method] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return sdk.AuthResult{}, nil
		}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
