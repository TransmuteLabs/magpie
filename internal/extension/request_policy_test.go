package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func fixtureRequestPolicy(ctx context.Context, call sdk.Call, stream *sdk.Stream, raw json.RawMessage) (any, error) {
	var input struct {
		Requests []sdk.RequestRecoveryRequest `json:"requests"`
		Output   bool                         `json:"output"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	if input.Output {
		if err := stream.Emit(ctx, "data", []byte("one output")); err != nil {
			return nil, err
		}
	}
	var decisions []sdk.RequestRecoveryResult
	for _, request := range input.Requests {
		decision, err := sdk.DecideRequest(ctx, stream, request)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func requestPolicyManager(t *testing.T) (*Manager, sdk.Scope) {
	t.Helper()
	directory := fixturePackage(t)
	path := filepath.Join(directory, sdk.ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Contract.Version.Minor = 12
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.RequestPolicy)
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: "fixture", Provider: "provider", Account: "account", Generation: 1, Operation: "request-policy-operation", Attempt: 1}
	if _, err := manager.Store().SaveAccount(Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"token":"synthetic"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	return manager, scope
}

func rateRecovery() sdk.RequestRecoveryRequest {
	return sdk.RequestRecoveryRequest{Step: "asset", Evidence: "read-only", Facts: sdk.RequestFailureFacts{Status: 429, Code: "pace", Operation: sdk.MethodToolCall, Phase: "response", Complete: true}, Rules: []sdk.RequestFailureRule{{ID: "pace-code", Status: []int{429}, Code: "pace", Class: "transient-rate", Scope: "account"}}}
}

func callRequestPolicy(ctx context.Context, manager *Manager, scope sdk.Scope, requests []sdk.RequestRecoveryRequest, output bool, consume func(Event) error) ([]sdk.RequestRecoveryResult, error) {
	value, _ := json.Marshal(map[string]any{"requests": requests, "output": output})
	args, _ := json.Marshal(map[string]any{"action": "request-policy", "value": json.RawMessage(value)})
	raw, err := manager.Call(ctx, scope.Plugin, sdk.MethodToolCall, scope, sdk.ToolRequest{ID: "tool", Arguments: args}, consume)
	var result []sdk.RequestRecoveryResult
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func TestRequestPolicyWorkerSharesOuterBudgetAndJournal(t *testing.T) {
	manager, scope := requestPolicyManager(t)
	zero, total := 0, 2
	ctx := requestpolicy.WithConfig(context.Background(), &requestpolicy.Config{Defaults: requestpolicy.Options{BaseDelayMS: &zero, MaxTotalRetries: &total}})
	request := rateRecovery()
	result, err := callRequestPolicy(ctx, manager, scope, []sdk.RequestRecoveryRequest{request, request, request}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 || !result[0].Retry || !result[1].Retry || result[2].Retry || result[2].Reason != "budget-exhausted" {
		t.Fatalf("%+v", result)
	}
	budget := requestpolicy.FromContext(ctx)
	policy, _ := budget.Policy("outer", requestpolicy.Defaults())
	if !errors.Is(budget.Admit(ctx, policy, requestpolicy.Decision{Retry: true}), requestpolicy.ErrDenied) {
		t.Fatal("outer acquired a fresh budget")
	}
	receipt, err := manager.Store().Operation(scope.Plugin, scope.Operation)
	if err != nil || receipt.State != "completed" || receipt.Scope != scope {
		t.Fatalf("journal changed: %+v %v", receipt, err)
	}
}

func TestRequestPolicyClassifiesWithoutEvidenceAndNeverRefreshes(t *testing.T) {
	manager, scope := requestPolicyManager(t)
	request := rateRecovery()
	request.Evidence = ""
	request.Facts.Status = 401
	request.Facts.Code = "invalid_request"
	request.Rules = []sdk.RequestFailureRule{{ID: "request-before-auth", Status: []int{401}, Code: "invalid_request", Class: "request-refusal", Scope: "request"}, {ID: "auth", Status: []int{401}, Class: "auth-refusal", Scope: "account"}}
	result, err := callRequestPolicy(context.Background(), manager, scope, []sdk.RequestRecoveryRequest{request}, false, nil)
	if err != nil || len(result) != 1 || result[0].Retry || result[0].Class != "request-refusal" || result[0].Rule != "request-before-auth" {
		t.Fatalf("%+v %v", result, err)
	}
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != 1 {
		t.Fatalf("classification refreshed account: %+v %v", account, err)
	}
}

func TestRequestPolicyRefusesAfterOutput(t *testing.T) {
	manager, scope := requestPolicyManager(t)
	outputs := 0
	_, err := callRequestPolicy(context.Background(), manager, scope, []sdk.RequestRecoveryRequest{rateRecovery()}, true, func(event Event) error {
		if event.Name == "data" {
			outputs++
		}
		return nil
	})
	if err == nil || outputs != 1 {
		t.Fatalf("output=%d err=%v", outputs, err)
	}
	receipt, err := manager.Store().Operation(scope.Plugin, scope.Operation)
	if err != nil || receipt.State != "uncertain" || receipt.Scope != scope {
		t.Fatalf("journal %+v %v", receipt, err)
	}
}

func TestRequestPolicyWorkerClassifiesSameStatusByConjunctiveFacts(t *testing.T) {
	manager, scope := requestPolicyManager(t)
	zero := 0
	ctx := requestpolicy.WithConfig(context.Background(), &requestpolicy.Config{Defaults: requestpolicy.Options{BaseDelayMS: &zero}})
	rules := []sdk.RequestFailureRule{
		{ID: "budget", Status: []int{403}, Contains: "quota exhausted", Class: "quota", Scope: "account"},
		{ID: "invalid-input", Status: []int{403}, Contains: "permission denied", Class: "request-refusal", Scope: "request"},
		{ID: "pace", Status: []int{403}, Code: "pace", Type: "temporary", Contains: "slow down", Headers: map[string]string{"X-Pace": "yes"}, Operation: sdk.MethodToolCall, Phase: "response", Class: "transient-rate", Scope: "account"},
	}
	var requests []sdk.RequestRecoveryRequest
	for _, message := range []string{"quota exhausted", "permission denied", "slow down", "slow down"} {
		request := rateRecovery()
		request.Step = "step" + string(rune('a'+len(requests)))
		request.Rules = rules
		request.Facts.Status = 403
		request.Facts.Code = "pace"
		request.Facts.Type = "temporary"
		request.Facts.Message = message
		request.Facts.Headers = map[string]string{"x-pace": "yes"}
		if len(requests) == 3 {
			request.Facts.Headers = nil
		}
		requests = append(requests, request)
	}
	results, err := callRequestPolicy(ctx, manager, scope, requests, false, nil)
	if err != nil || len(results) != 4 {
		t.Fatalf("%+v %v", results, err)
	}
	for i, want := range []string{"quota", "request-refusal", "transient-rate", "unknown"} {
		if results[i].Class != want || results[i].Retry != (i == 2) {
			t.Fatalf("case %d: %+v", i, results[i])
		}
	}
}

func TestRequestPolicyWorkerReportsTypedNextAllowedWithoutWaitingPastBudget(t *testing.T) {
	manager, scope := requestPolicyManager(t)
	maximum := 1
	ctx := requestpolicy.WithConfig(context.Background(), &requestpolicy.Config{Defaults: requestpolicy.Options{MaxDelayMS: &maximum}})
	request := rateRecovery()
	duration := int64(10000)
	request.Facts.RetryAfterMS = &duration
	before := time.Now()
	results, err := callRequestPolicy(ctx, manager, scope, []sdk.RequestRecoveryRequest{request}, false, nil)
	if err != nil || len(results) != 1 {
		t.Fatalf("%+v %v", results, err)
	}
	next, err := time.Parse(time.RFC3339Nano, results[0].NextAllowedAt)
	if err != nil || results[0].Retry || results[0].Reason != "delay-limit" || next.Before(before.Add(10*time.Second)) {
		t.Fatalf("%+v %v", results[0], err)
	}
}
