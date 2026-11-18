package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type requestPolicyDiagnosticContext struct {
	context.Context
	budget *requestpolicy.Budget
	revoke func()
	once   sync.Once
}

func (current *requestPolicyDiagnosticContext) Done() <-chan struct{} {
	if used, _ := current.budget.Counts(""); used > 0 {
		current.once.Do(current.revoke)
	}
	return current.Context.Done()
}

func TestRequestPolicyRevocationDiagnosticsUseCommonOwner(test *testing.T) {
	for _, phase := range []string{"before-admission", "during-wait"} {
		for _, cause := range []string{"disable", "configuration", "account", "lease", "network", "window", "canceled", "output"} {
			test.Run(phase+"/"+cause, func(test *testing.T) {
				manager, scope := requestPolicyManager(test)
				lease, err := manager.Acquire(test.Context(), scope.Plugin)
				if err != nil {
					test.Fatal(err)
				}
				defer lease.Close()
				zero := 0
				parent, cancel := context.WithCancel(test.Context())
				defer cancel()
				var windowRevoked atomic.Bool
				parent = context.WithValue(parent, windowAdmissionKey{}, func(context.Context) error {
					if windowRevoked.Load() {
						return errors.New("PRIVATE_WINDOW")
					}
					return nil
				})
				ctx := beginRequestPolicy(requestpolicy.WithConfig(parent, &requestpolicy.Config{
					Defaults: requestpolicy.Options{BaseDelayMS: &zero},
				}), lease, scope, sdk.MethodToolCall, nil)
				current := &requestPolicyDiagnosticContext{Context: ctx, budget: requestpolicy.FromContext(ctx)}
				current.revoke = func() {
					switch cause {
					case "disable":
						if err := manager.SetEnabled(test.Context(), scope.Plugin, false); err != nil {
							test.Fatal(err)
						}
					case "configuration":
						if _, err := manager.CompareAndSwapModuleConfig(test.Context(), scope.Plugin, lease.ConfigVersion(), json.RawMessage(`{}`)); err != nil {
							test.Fatal(err)
						}
					case "account":
						if _, err := manager.Store().SaveAccount(Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"token":"PRIVATE_ACCOUNT"}`)}, scope.Generation); err != nil {
							test.Fatal(err)
						}
					case "lease":
						lease.Close()
					case "network":
						current.Context = netproxy.Hold(current.Context, errors.New("PRIVATE_NETWORK"))
					case "window":
						windowRevoked.Store(true)
					case "canceled":
						cancel()
					case "output":
						invocation := ctx.Value(requestPolicyKey{}).(*requestPolicyInvocation)
						invocation.mu.Lock()
						invocation.output = true
						invocation.mu.Unlock()
					}
				}
				var records []moduleDiagnostic
				manager.diagnosticLog = func(record moduleDiagnostic) {
					records = append(records, record)
				}
				request := rateRecovery()
				delay := int64(1)
				request.Facts.RetryAfterMS = &delay
				request.Facts.Message = "PRIVATE_MESSAGE"
				request.Facts.Headers = map[string]string{"Authorization": "PRIVATE_HEADER"}
				raw, err := json.Marshal(request)
				if err != nil {
					test.Fatal(err)
				}
				if phase == "before-admission" {
					current.once.Do(current.revoke)
				}
				result, err := manager.recoverRequest(current, scope, raw, hostAuthority{manifest: lease.Manifest(), method: sdk.MethodToolCall})
				if err == nil || result != nil {
					test.Fatalf("revoked callback acquired a result/permit: result=%+v err=%v", result, err)
				}
				if len(records) != 1 || records[0].RequestPolicy == nil {
					test.Fatalf("revoked recovery diagnostic count=%d want=1: %+v", len(records), records)
				}
				record := records[0]
				diagnostic := record.RequestPolicy
				reason := "revoked"
				if cause == "canceled" {
					reason = "canceled"
				} else if cause == "output" {
					reason = "unsafe"
				}
				wantRetries := 0
				if phase == "during-wait" {
					wantRetries = 1
				}
				if record.Plugin != scope.Plugin || record.Provider != scope.Provider || record.Account != scope.Account ||
					record.Operation != scope.Operation || record.Attempt != scope.Attempt ||
					diagnostic.Step != request.Step || diagnostic.Action != requestpolicy.ActionTerminal || diagnostic.Retry ||
					diagnostic.Reason != reason || diagnostic.TotalRetries != wantRetries || diagnostic.ProviderRetries != wantRetries {
					test.Fatalf("untruthful revocation diagnostic: %+v result=%+v", record, diagnostic)
				}
				if phase == "before-admission" && (diagnostic.Class != "unknown" || diagnostic.Scope != "unknown" || diagnostic.Rule != "") {
					test.Fatalf("admission failure invented classification: %+v", diagnostic)
				}
				encoded, _ := json.Marshal(record)
				if strings.Contains(string(encoded), "PRIVATE_") {
					test.Fatal("revocation diagnostic persisted private evidence")
				}
				if used, _ := current.budget.Counts(""); used != wantRetries {
					test.Fatalf("diagnostic changed retry admission: %d want=%d", used, wantRetries)
				}
			})
		}
	}
}

func TestRequestPolicyInvalidRecoveryDoesNotInventDiagnostic(test *testing.T) {
	manager, scope := requestPolicyManager(test)
	lease, err := manager.Acquire(test.Context(), scope.Plugin)
	if err != nil {
		test.Fatal(err)
	}
	defer lease.Close()
	ctx := beginRequestPolicy(test.Context(), lease, scope, sdk.MethodToolCall, nil)
	recorded := 0
	manager.diagnosticLog = func(moduleDiagnostic) { recorded++ }
	raw, _ := json.Marshal(rateRecovery())
	authority := hostAuthority{manifest: lease.Manifest(), method: sdk.MethodToolCall}
	stale := scope
	stale.Generation++
	for _, scenario := range []struct {
		ctx   context.Context
		scope sdk.Scope
		raw   json.RawMessage
	}{
		{test.Context(), scope, raw},
		{ctx, stale, raw},
		{ctx, scope, json.RawMessage(`{"step":"asset","PRIVATE_BODY":true}`)},
	} {
		if result, err := manager.recoverRequest(scenario.ctx, scenario.scope, scenario.raw, authority); err == nil || result != nil {
			test.Fatalf("invalid recovery acquired authority: %+v %v", result, err)
		}
	}
	if recorded != 0 {
		test.Fatalf("invalid callback invented %d policy diagnostics", recorded)
	}
}
