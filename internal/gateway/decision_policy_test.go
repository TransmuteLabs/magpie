package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestDecisionCalibrationFallbackKeepsOperation(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			fresh(t)
			upstream := &decideFleet{status: map[string]int{"ka1": 503}}
			serverHTTP := upstream.serve()
			t.Cleanup(serverHTTP.Close)
			selected := provider.Provider{ID: "decision", Name: "Decision", Key: "ka1",
				Keys: []provider.KeyAccount{{Key: "ka2"}}, Routing: provider.Ordered,
				Decide: serverHTTP.URL + "/v1", Models: []string{"jev-latest"}}
			if err := provider.Save(selected); err != nil {
				t.Fatal(err)
			}
			ctx := ownPolicyContext(t, total)
			server := New()
			t.Cleanup(server.idle.stop)
			verdict, err := server.askDeciderWithContext(ctx, time.Minute, selected, "jev-latest",
				[]string{"feature", "bug"}, before{}, false, "fix this")
			actual := strings.Join(upstream.took(), ",")
			expected := "ka1"
			if total == 1 {
				expected = "ka1,ka2,ka2"
			}
			if actual != expected || (err == nil) != (total == 1) || total == 1 && verdict.Intent != "bug" {
				t.Fatalf("calibration fallback reset operation: total=%d sends=%s verdict=%+v err=%v", total, actual, verdict, err)
			}
			if used, selected := requestpolicy.FromContext(ctx).Counts("decision"); used != total || selected != total {
				t.Fatal("calibration fallback reset root/provider budget", used, selected)
			}
		})
	}
}

func TestDecisionSharedPolicyTerminalBudgetAndAccounting(t *testing.T) {
	for _, path := range []string{"/v1/systemone", "/v1/decisions"} {
		for _, scenario := range []string{"request", "content", "root-budget", "model-budget", "root-dispatch", "model-dispatch", "sibling", "accepted"} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				var mutex sync.Mutex
				var accounts []string
				id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
					var selected struct{ Account string }
					if err := json.NewDecoder(request.Body).Decode(&selected); err != nil {
						t.Error(err)
						return
					}
					mutex.Lock()
					accounts = append(accounts, selected.Account)
					mutex.Unlock()
					if selected.Account == "alice" {
						class, scope := "quota", "account"
						if scenario == "request" {
							class, scope = "request-refusal", "request"
						} else if scenario == "content" {
							class, scope = "content-refusal", "request"
						}
						reply := policyFixtureRefusal(class, scope, nil, http.StatusTooManyRequests)
						if scenario == "accepted" {
							reply["accepted"] = true
						}
						json.NewEncoder(writer).Encode(reply)
						return
					}
					writer.Write([]byte(`{}`))
				}, func(manifest *sdk.Manifest) {
					manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ModelDiscovery)
					manifest.Providers[0].Operations = []sdk.ProviderOperation{sdk.OperationSystemOne}
					manifest.Providers[0].Models = append(manifest.Providers[0].Models,
						sdk.Model{ID: "decision", Kind: "decision", Operations: []sdk.ProviderOperation{sdk.OperationSystemOne}})
				})
				selected, err := provider.Find(id)
				if err != nil {
					t.Fatal(err)
				}
				selected.Routing = provider.Ordered
				if err := provider.Save(*selected); err != nil {
					t.Fatal(err)
				}
				zero, total := 0, 3
				current := settings.Load()
				current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
					MaxTotalRetries: &total, BaseDelayMS: &zero, ShortWaitMS: &zero,
				}}
				if scenario == "root-budget" || scenario == "root-dispatch" {
					current.RequestPolicy.Defaults.MaxTotalRetries = &zero
				} else if scenario == "model-budget" || scenario == "model-dispatch" {
					current.RequestPolicy.Defaults.Models = map[string]requestpolicy.Options{
						"decision": {MaxTotalRetries: &zero},
					}
				}
				if err := settings.Save(current); err != nil {
					t.Fatal(err)
				}
				server := New()
				t.Cleanup(server.idle.stop)
				body := `{"model":"` + id + `/decision","state":"test","questions":{"safe":{"type":"noul"}}}`
				if path == "/v1/decisions" {
					body = `{"model":"` + id + `/decision","input":"test","questions":[{"name":"safe","type":"predicate"}]}`
				}
				ctx := withRequestPolicy(context.Background())
				if strings.HasSuffix(scenario, "dispatch") {
					ctx = requestpolicy.WithSelection(ctx, "", "decision")
					policy, err := requestPolicy(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if err := requestpolicy.FromContext(ctx).Dispatch(ctx, policy); err != nil {
						t.Fatal(err)
					}
				}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
				request.RemoteAddr = "127.0.0.1:23456"
				response := httptest.NewRecorder()
				server.Front().ServeHTTP(response, request)
				mutex.Lock()
				actual := append([]string(nil), accounts...)
				mutex.Unlock()
				wantStatus, wantSends := http.StatusTooManyRequests, 1
				if scenario == "sibling" {
					wantStatus, wantSends = http.StatusOK, 2
				} else if scenario == "accepted" {
					wantStatus = http.StatusConflict
				} else if strings.HasSuffix(scenario, "budget") {
					wantStatus = http.StatusTooManyRequests
				}
				if strings.HasSuffix(scenario, "dispatch") {
					wantSends = 0
				}
				if response.Code != wantStatus || len(actual) != wantSends {
					t.Fatalf("decision policy bypassed: status=%d accounts=%v want status=%d sends=%d body=%s",
						response.Code, actual, wantStatus, wantSends, response.Body)
				}
				calls, rows := server.Recent(), usage.Load(time.Time{})
				if len(calls) != 1 || len(rows) != 1 || rows[0].Status != wantStatus || calls[0].Status != wantStatus {
					t.Fatalf("decision accounting lost or duplicated: calls=%+v rows=%+v", calls, rows)
				}
				if scenario == "sibling" && (rows[0].Input != 41 || rows[0].Output != 3 ||
					calls[0].Usage.Input != 41 || calls[0].Usage.Output != 3) {
					t.Fatal("decision fallback changed successful usage", calls, rows)
				}
				if strings.HasSuffix(scenario, "dispatch") && !rows[0].Rejected {
					t.Fatal("denied dispatch was not recorded as rejected", rows)
				}
			})
		}
	}
}

func TestSharedPolicyBudgetRefusalKeepsOrdinaryLedger(t *testing.T) {
	for _, scope := range []string{"root", "model"} {
		t.Run(scope, func(t *testing.T) {
			var mutex sync.Mutex
			sends := 0
			id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				mutex.Lock()
				sends++
				mutex.Unlock()
				json.NewEncoder(writer).Encode(policyFixtureRefusal("quota", "account", nil, http.StatusTooManyRequests))
			})
			zero := 0
			options := requestpolicy.Options{MaxTotalRetries: &zero, BaseDelayMS: &zero, ShortWaitMS: &zero}
			if scope == "model" {
				options.MaxTotalRetries = nil
				options.Models = map[string]requestpolicy.Options{"free": {MaxTotalRetries: &zero}}
			}
			current := settings.Load()
			current.RequestPolicy = &requestpolicy.Config{Defaults: options}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			response := requestPolicyFront(t, `{"model":"`+id+`/free","messages":[{"role":"user","content":"budget"}]}`)
			mutex.Lock()
			actual := sends
			mutex.Unlock()
			rows := usage.Load(time.Time{})
			if response.Code != 429 || actual != 1 || len(rows) != 1 || rows[0].Status != 429 {
				t.Fatalf("budget refusal lost ordinary ledger: status=%d sends=%d rows=%+v", response.Code, actual, rows)
			}
		})
	}
}
