package extension

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestRequestPolicyRejectsBusinessReplayAfterAcceptance(t *testing.T) {
	manager, scope := requestPolicyManager(t)
	zero := 0
	ctx := requestpolicy.WithConfig(context.Background(), &requestpolicy.Config{
		Defaults: requestpolicy.Options{BaseDelayMS: &zero},
	})
	request := rateRecovery()
	request.Evidence = "rejected-before-effect"
	results, err := callRequestPolicy(ctx, manager, scope, []sdk.RequestRecoveryRequest{request}, false, nil)
	if err != nil || len(results) != 1 {
		t.Fatalf("accepted invocation could not classify its refusal: %+v %v", results, err)
	}
	if results[0].Retry || results[0].Reason != "unsafe" || results[0].TotalRetries != 0 {
		t.Fatalf("accepted business operation received a repeat permit: %+v", results[0])
	}
	receipt, err := manager.Store().Operation(scope.Plugin, scope.Operation)
	if err != nil || receipt.State != "completed" || receipt.Scope != scope {
		t.Fatalf("refusal classification changed the accepted journal: %+v %v", receipt, err)
	}
}

func TestRequestPolicyKeepsSelectedModelLimits(t *testing.T) {
	for _, limit := range []string{"maxRetries", "shortWaitMs", "otherModel"} {
		t.Run(limit, func(t *testing.T) {
			manager, scope := requestPolicyManager(t)
			zero := 0
			model, options := "model", requestpolicy.Options{MaxRetries: &zero}
			if limit == "shortWaitMs" {
				options = requestpolicy.Options{ShortWaitMS: &zero}
			} else if limit == "otherModel" {
				model = "other"
			}
			ctx := requestpolicy.WithConfig(context.Background(), &requestpolicy.Config{
				Defaults: requestpolicy.Options{
					BaseDelayMS: &zero,
					Models:      map[string]requestpolicy.Options{model: options},
				},
			})
			request := rateRecovery()
			delay := int64(1)
			request.Facts.Operation = sdk.MethodProviderExecute
			request.Facts.RetryAfterMS = &delay
			value, err := json.Marshal(map[string]any{
				"requestPolicy": map[string]any{"requests": []sdk.RequestRecoveryRequest{request}},
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderExecute, scope,
				sdk.ExecuteRequest{Protocol: "chat", Model: "model", Options: value}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Decisions []sdk.RequestRecoveryResult `json:"decisions"`
			}
			if err := json.Unmarshal(raw, &result); err != nil || len(result.Decisions) != 1 {
				t.Fatalf("invalid model policy result: %s %v", raw, err)
			}
			results := result.Decisions
			wantRetry, wantUsed := limit == "otherModel", 0
			if wantRetry {
				wantUsed = 1
			}
			if results[0].Retry != wantRetry || results[0].TotalRetries != wantUsed {
				t.Fatalf("selected model lost %s policy: %+v", limit, results[0])
			}
		})
	}
}
