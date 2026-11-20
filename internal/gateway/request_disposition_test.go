package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

// Raw result JSON deliberately lets the same ordinary worker regression run
// against the predecessor which has no final-refusal DTO yet.
const policyRouteWorker = `package main
import("context";"encoding/json";"net/http";"os";"bytes";"io"; sdk "github.com/yetone/magpie/pkg/extension")
func main(){
 raw,_:=os.ReadFile(sdk.ManifestFile);var manifest sdk.Manifest;json.Unmarshal(raw,&manifest)
 module:=sdk.Module{Manifest:manifest,Handlers:map[string]sdk.Handler{
 sdk.MethodProviderExecute:func(ctx context.Context,call sdk.Call,stream *sdk.Stream)(any,error){
  var request sdk.ExecuteRequest;if err:=json.Unmarshal(call.Params,&request);err!=nil{return nil,err}
  var auth struct{URL string};if err:=json.Unmarshal(call.Auth,&auth);err!=nil{return nil,err}
  body,_:=json.Marshal(map[string]any{"account":call.Scope.Account,"model":request.Model,"operation":call.Scope.Operation,"protocol":request.Protocol,"body":request.Body})
  req,_:=http.NewRequestWithContext(ctx,"POST",auth.URL,bytes.NewReader(body));res,err:=http.DefaultClient.Do(req);if err!=nil{return nil,err}
  raw,err:=io.ReadAll(res.Body);res.Body.Close();if err!=nil{return nil,err}
  var reply map[string]any;if err:=json.Unmarshal(raw,&reply);err!=nil{return nil,err}
  if chunk, ok := reply["chatStream"].(string); ok {
   if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
    return nil, err
   }
   headers := sdk.HeadersEvent{Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/event-stream"}}}
   if err := stream.Emit(ctx, "headers", headers); err != nil {
    return nil, err
   }
   if chunk != "" {
    if err := stream.Emit(ctx, "data", []byte(chunk)); err != nil {
     return nil, err
    }
   }
   return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed"}, nil
  }
  if reply["accepted"]==true {if err:=stream.Emit(ctx,"accepted",sdk.AcceptedEvent{OperationID:call.Scope.Operation});err!=nil{return nil,err};delete(reply,"accepted")}
  if reply["refusal"]!=nil{
   switch reply["handoffEvent"] {
   case "headers":
    if err:=stream.Emit(ctx,"headers",sdk.HeadersEvent{Status:http.StatusTooManyRequests});err!=nil{return nil,err}
   case "usage":
    if err:=stream.Emit(ctx,"usage",sdk.Usage{InputTokens:17});err!=nil{return nil,err}
   }
   delete(reply,"handoffEvent")
   return reply,nil
  }
  if err:=stream.Emit(ctx,"accepted",sdk.AcceptedEvent{OperationID:call.Scope.Operation});err!=nil{return nil,err}
  content,_:=json.Marshal(map[string]any{"id":"fixture","model":request.Model,"choices":[]any{map[string]any{"index":0,"message":map[string]string{"role":"assistant","content":call.Scope.Account},"finish_reason":"stop"}}})
  var usage *sdk.Usage
  if request.Operation=="systemone" {content=[]byte("{\"answers\":{\"safe\":{\"type\":\"noul\",\"noul\":0.93},\"intent\":{\"choice\":\"safe\",\"confidence\":0.93},\"levels\":{\"noul\":0}},\"usage\":{\"input_tokens\":41,\"output_tokens\":3}}");usage=&sdk.Usage{InputTokens:41,OutputTokens:3}}
  if err:=stream.Emit(ctx,"headers",sdk.HeadersEvent{Status:200,Headers:http.Header{"Content-Type":{"application/json"}}});err!=nil{return nil,err}
  if err:=stream.Emit(ctx,"data",content);err!=nil{return nil,err}
  return sdk.ExecuteResult{Status:200,Outcome:"completed",Usage:usage},nil
 }}}
 if manifest.Contract.Has(sdk.ModelDiscovery){module.Handlers[sdk.MethodProviderModels]=func(context.Context,sdk.Call,*sdk.Stream)(any,error){return sdk.ModelsResult{Models:manifest.Providers[0].Models},nil}}
 for _,method:=range sdk.RequiredMethods(manifest){if module.Handlers[method]==nil{module.Handlers[method]=func(context.Context,sdk.Call,*sdk.Stream)(any,error){return nil,&sdk.Error{Code:"not_supported",Message:"fixture does not sign in",Outcome:"not-started"}}}}
 if sdk.Serve(context.Background(),os.Stdin,os.Stdout,module)!=nil{os.Exit(1)}
}`

func installPolicyRouteFixture(t *testing.T, handler http.HandlerFunc, options ...func(*sdk.Manifest)) string {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_ADDR", "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /execute", handler)
	vendor := httptest.NewServer(mux)
	t.Cleanup(vendor.Close)
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	source := filepath.Join(t.TempDir(), "worker.go")
	if err := os.WriteFile(source, []byte(policyRouteWorker), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o", filepath.Join(directory, entry), source)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build policy fixture: %v %s", err, out)
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.policy", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract:  sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 12}, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth, sdk.RequestPolicy}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []protocolext.Protocol{"chat"}, Models: []sdk.Model{{ID: "free", Protocols: []protocolext.Protocol{"chat"}}, {ID: "paid", Protocols: []protocolext.Protocol{"chat"}}}}}}
	for _, option := range options {
		option(&manifest)
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(ctx, directory); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]string{"url": vendor.URL + "/execute"})
	for _, account := range []string{"alice", "bob"} {
		if _, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: manifest.ID, Provider: "vendor", ID: account, Auth: auth}, 0); err != nil {
			t.Fatal(err)
		}
	}
	provider.Changed()
	if manifest.Contract.Has(sdk.ModelDiscovery) {
		accounts, err := provider.ExtensionProviders()
		if err != nil {
			t.Fatal(err)
		}
		for _, account := range accounts {
			if _, err := account.Fetch(ctx); err != nil {
				t.Fatal(err)
			}
		}
		provider.Changed()
	}
	return provider.ExtensionProviderID(manifest.ID, "vendor")
}

func policyFixtureRefusal(class, scope string, windows []any, statuses ...int) map[string]any {
	status := http.StatusForbidden
	if len(statuses) > 0 {
		status = statuses[0]
	}
	return map[string]any{"status": status, "outcome": "not-started", "refusal": map[string]any{
		"step": "inference", "evidence": "rejected-before-effect",
		"facts": map[string]any{"status": status, "code": "denied", "message": "fixture refusal", "operation": "provider.execute", "phase": "response", "complete": true},
		"rules": []any{map[string]any{"id": "fixture.rule", "status": []int{status}, "code": "denied", "class": class, "scope": scope}}, "windows": windows}}
}

func TestRequestDispositionInvalidRequestDoesNotEnumerateAccounts(t *testing.T) {
	var mutex sync.Mutex
	var accounts []string
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Account string }
		json.NewDecoder(r.Body).Decode(&input)
		mutex.Lock()
		accounts = append(accounts, input.Account)
		mutex.Unlock()
		json.NewEncoder(w).Encode(policyFixtureRefusal("request-refusal", "request", nil))
	})
	response := mediaFixtureCall(New().Front(), "POST", "/v1/chat/completions", `{"model":"`+id+`/free","messages":[{"role":"user","content":"hi"}]}`)
	mutex.Lock()
	defer mutex.Unlock()
	if response.Code != 403 || len(accounts) != 1 {
		t.Fatalf("status=%d accounts=%v body=%s", response.Code, accounts, response.Body)
	}
}

func TestRequestDispositionLocalQueueHandoff(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		single     bool
		noRetries  bool
		accepted   bool
		undeclared bool
		wantStatus int
		wantCalls  string
	}{
		{name: "ready sibling", wantStatus: 200, wantCalls: "alice,bob"},
		{name: "no eligible candidate", single: true, wantStatus: 429, wantCalls: "alice"},
		{name: "exhausted dispatch budget", noRetries: true, wantStatus: 429, wantCalls: "alice"},
		{name: "accepted workflow", accepted: true, wantStatus: 409, wantCalls: "alice"},
		{name: "missing capability", undeclared: true, wantStatus: 409, wantCalls: "alice"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var mutex sync.Mutex
			var calls []string
			result := sdk.HandoffExecution("local-queue", "queue-timeout", "local queue exhausted", new(int64(5000)))
			reply, err := json.Marshal(struct {
				sdk.ExecuteResult
				Accepted bool `json:"accepted,omitempty"`
			}{result, scenario.accepted})
			if err != nil {
				t.Fatal(err)
			}
			id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				var input struct{ Account string }
				if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				mutex.Lock()
				calls = append(calls, input.Account)
				mutex.Unlock()
				if input.Account == "alice" {
					writer.Write(reply)
					return
				}
				io.WriteString(writer, `{}`)
			}, func(manifest *sdk.Manifest) {
				if !scenario.undeclared {
					manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.RequestHandoff)
				}
			})
			if scenario.single {
				if err := extensionhost.Default().Store().DeleteAccount("fixture.policy", "vendor", "bob", 1); err != nil {
					t.Fatal(err)
				}
				provider.Changed()
			}
			if scenario.noRetries {
				zero := 0
				current := settings.Load()
				current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{MaxTotalRetries: &zero}}
				if err := settings.Save(current); err != nil {
					t.Fatal(err)
				}
			}

			server := New()
			t.Cleanup(server.idle.stop)
			body := `{"model":"` + id + `/free","messages":[{"role":"user","content":"local queue"}]}`
			response := mediaFixtureCall(server.Front(), "POST", "/v1/chat/completions", body)
			mutex.Lock()
			gotCalls := strings.Join(calls, ",")
			mutex.Unlock()
			if response.Code != scenario.wantStatus || gotCalls != scenario.wantCalls {
				t.Fatalf("status=%d calls=%s body=%s", response.Code, gotCalls, response.Body)
			}
			if scenario.wantStatus == http.StatusOK && !strings.Contains(response.Body.String(), `"content":"bob"`) {
				t.Fatalf("ready sibling did not serve reply: %s", response.Body)
			}
			if scenario.wantStatus == 429 && (response.Header().Get("Retry-After") == "" || !strings.Contains(response.Body.String(), "local admission capacity")) {
				t.Fatalf("missing local retry hint: headers=%v body=%s", response.Header(), response.Body)
			}
			if scenario.accepted || scenario.undeclared {
				return
			}
			tries := lastRoute(server).Tries
			if len(tries) != strings.Count(scenario.wantCalls, ",")+1 || tries[0].Status != 429 || tries[0].Again != 0 ||
				tries[0].Rest != nil || tries[0].UpstreamFailure != (usage.UpstreamFailure{}) {
				t.Fatalf("local handoff waited, rested, or looked like an upstream failure: %+v", tries)
			}
			records := decisionUsageRecords(t)
			if len(records) != 1 || records[0].Status != scenario.wantStatus || records[0].UpstreamFailure != (usage.UpstreamFailure{}) {
				t.Fatalf("request usage must keep final status without an upstream failure: %+v", records)
			}
			if scenario.single {
				second := mediaFixtureCall(server.Front(), "POST", "/v1/chat/completions", body)
				mutex.Lock()
				gotCalls = strings.Join(calls, ",")
				mutex.Unlock()
				if second.Code != 429 || gotCalls != "alice,alice" {
					t.Fatalf("independent request must reach local queue: status=%d calls=%s body=%s", second.Code, gotCalls, second.Body)
				}
			}
		})
	}
}

func TestRequestDispositionLocalHandoffReplayFence(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		event      string
		usage      *sdk.Usage
		undeclared bool
		incomplete bool
	}{
		{name: "published headers", event: "headers"},
		{name: "published usage", event: "usage"},
		{name: "final usage", usage: &sdk.Usage{InputTokens: 17}},
		{name: "undeclared capability", undeclared: true},
		{name: "incomplete facts", incomplete: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := sdk.HandoffExecution("local-queue", "queue-timeout", "local queue exhausted", nil)
			result.Usage = scenario.usage
			result.Refusal.Facts.Complete = !scenario.incomplete
			reply, err := json.Marshal(struct {
				sdk.ExecuteResult
				HandoffEvent string `json:"handoffEvent,omitempty"`
			}{result, scenario.event})
			if err != nil {
				t.Fatal(err)
			}
			var mutex sync.Mutex
			var calls []struct{ Account, Operation string }
			id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				var call struct{ Account, Operation string }
				if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				mutex.Lock()
				calls = append(calls, call)
				mutex.Unlock()
				if call.Account == "alice" {
					writer.Write(reply)
				} else {
					io.WriteString(writer, `{}`)
				}
			}, func(manifest *sdk.Manifest) {
				if !scenario.undeclared {
					manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.RequestHandoff)
				}
			})
			server := New()
			t.Cleanup(server.idle.stop)
			ctx := extensionhost.WithRequestMetadata(t.Context(), extensionhost.RequestMetadata{
				Plugin: "fixture.policy", RequestID: "stable-handoff-request", CallerID: "fixture-caller",
			})
			send := func() *httptest.ResponseRecorder {
				body := `{"model":"` + id + `/free","messages":[{"role":"user","content":"handoff fence"}]}`
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
				request.RemoteAddr = "127.0.0.1:12345"
				response := httptest.NewRecorder()
				server.Front().ServeHTTP(response, request)
				return response
			}
			first := send()
			if first.Code != http.StatusConflict || first.Header().Get("X-Should-Retry") != "false" {
				t.Errorf("contradictory handoff reached fallback or lost the replay fence: status=%d body=%s", first.Code, first.Body)
			}
			mutex.Lock()
			operation := ""
			if len(calls) > 0 && calls[0].Account == "alice" {
				operation = calls[0].Operation
			}
			mutex.Unlock()
			receipt, err := extensionhost.Default().Store().Operation("fixture.policy", operation)
			if err != nil || receipt.State != "uncertain" {
				t.Errorf("contradictory handoff must persist an uncertain receipt: state=%q err=%v", receipt.State, err)
			}
			repeated := send()
			if repeated.Code != http.StatusConflict {
				t.Errorf("same request ID lost its durable replay fence: status=%d body=%s", repeated.Code, repeated.Body)
			}
			mutex.Lock()
			defer mutex.Unlock()
			if len(calls) != 1 {
				t.Errorf("contradictory handoff reached a sibling or replayed the original account: %+v", calls)
			}
		})
	}
}

func TestRequestDispositionFreeWindowLeavesPaidAndSiblingEligible(t *testing.T) {
	var mutex sync.Mutex
	var calls []string
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Account, Model string }
		json.NewDecoder(r.Body).Decode(&input)
		mutex.Lock()
		calls = append(calls, input.Account+"/"+input.Model)
		mutex.Unlock()
		if input.Account == "alice" && input.Model == "free" {
			window := map[string]any{"id": "free-window", "models": []string{"free"}, "resetAt": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)}
			json.NewEncoder(w).Encode(policyFixtureRefusal("quota", "model", []any{window}))
			return
		}
		io.WriteString(w, `{}`)
	})
	handler := New().Front()
	for _, model := range []string{"free", "paid", "free"} {
		response := mediaFixtureCall(handler, "POST", "/v1/chat/completions", `{"model":"`+id+`/`+model+`","messages":[{"role":"user","content":"hi `+model+`"}]}`)
		if response.Code != 200 {
			t.Fatalf("%s status=%d body=%s", model, response.Code, response.Body)
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if got := strings.Join(calls, ","); got != "alice/free,bob/free,alice/paid,bob/free" {
		t.Fatalf("wrong account/model availability: %s", got)
	}
}

func TestRequestDispositionShortWaitHonorsActualDispatchTime(t *testing.T) {
	var mutex sync.Mutex
	var accounts []string
	var first, second, reset time.Time
	id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		var input struct{ Account string }
		json.NewDecoder(request.Body).Decode(&input)
		mutex.Lock()
		accounts = append(accounts, input.Account)
		number := len(accounts)
		if number == 1 {
			first = time.Now()
			reset = first.Add(150 * time.Millisecond)
		} else {
			second = time.Now()
		}
		deadline := reset
		mutex.Unlock()
		if number == 1 {
			json.NewEncoder(writer).Encode(policyFixtureRefusal("transient-rate", "account", []any{map[string]any{"id": "pace", "resetAt": deadline.UTC().Format(time.RFC3339Nano)}}))
			return
		}
		io.WriteString(writer, `{}`)
	})
	current := settings.Load()
	if err := json.Unmarshal([]byte(`{"requestPolicy":{"defaults":{"baseDelayMs":0,"shortWaitMs":500,"maxDelayMs":1000,"maxRetries":1,"maxTotalRetries":1}}}`), &current); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	response := mediaFixtureCall(New().Front(), "POST", "/v1/chat/completions", `{"model":"`+id+`/free","messages":[{"role":"user","content":"short wait"}]}`)
	mutex.Lock()
	defer mutex.Unlock()
	if response.Code != 200 || strings.Join(accounts, ",") != "alice,alice" || second.Before(reset) {
		t.Fatalf("status=%d accounts=%v elapsed=%v minimum=%v body=%s", response.Code, accounts, second.Sub(first), reset.Sub(first), response.Body)
	}
}

func TestRequestDispositionAllUnavailableReturnsTimedRefusal(t *testing.T) {
	var mutex sync.Mutex
	var accounts []string
	id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		var input struct{ Account string }
		json.NewDecoder(request.Body).Decode(&input)
		mutex.Lock()
		accounts = append(accounts, input.Account)
		mutex.Unlock()
		json.NewEncoder(writer).Encode(policyFixtureRefusal("quota", "model", []any{map[string]any{"id": "free", "models": []string{"free"}, "resetAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}}))
	})
	response := mediaFixtureCall(New().Front(), "POST", "/v1/chat/completions", `{"model":"`+id+`/free","messages":[{"role":"user","content":"all exhausted"}]}`)
	mutex.Lock()
	defer mutex.Unlock()
	if response.Code != 403 || strings.Join(accounts, ",") != "alice,bob" || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d accounts=%v headers=%v", response.Code, accounts, response.Header())
	}
}

func TestRequestDispositionAcceptedNeverSwitchesAccount(t *testing.T) {
	var mutex sync.Mutex
	var accounts []string
	id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		var input struct{ Account string }
		json.NewDecoder(request.Body).Decode(&input)
		mutex.Lock()
		accounts = append(accounts, input.Account)
		mutex.Unlock()
		result := policyFixtureRefusal("quota", "account", nil)
		result["accepted"] = true
		json.NewEncoder(writer).Encode(result)
	})
	response := mediaFixtureCall(New().Front(), "POST", "/v1/chat/completions", `{"model":"`+id+`/free","messages":[{"role":"user","content":"accepted"}]}`)
	mutex.Lock()
	defer mutex.Unlock()
	if response.Code != 409 || len(accounts) != 1 {
		t.Fatalf("status=%d accounts=%v body=%s", response.Code, accounts, response.Body)
	}
}

func TestFinalRefusalGroupCanLeaveSpentProvider(t *testing.T) {
	var first atomic.Int32
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		json.NewEncoder(w).Encode(policyFixtureRefusal("quota", "account", nil))
	})
	second := &scripted{replies: []reply{{200, "", chatOK}}}
	scriptedOn(t, "second", provider.Chat, second)
	if err := provider.SaveGroup(provider.Group{ID: "budget-group", Name: "Budget group", Members: []string{id + "/free", "second/m"}, Routing: "order"}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	current := settings.Load()
	current.RequestPolicy = &requestpolicy.Config{Providers: map[string]requestpolicy.Options{id: {MaxTotalRetries: &zero}}}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	response := requestPolicyFront(t, `{"model":"budget-group","messages":[{"role":"user","content":"hello"}]}`)
	if response.Code != 200 || first.Load() != 1 || second.n != 1 {
		t.Fatalf("status=%d sends=(%d,%d) body=%s", response.Code, first.Load(), second.n, response.Body.String())
	}
}

func TestDecisionClassifierFinalRefusal(t *testing.T) {
	for _, scenario := range []struct {
		name, class, scope            string
		cached, noEvidence, calibrate bool
		noRetries                     bool
		wantSecond                    int32
	}{
		{name: "request", class: "request-refusal", scope: "request"},
		{name: "unknown scope", class: "quota", scope: "unknown"},
		{name: "missing evidence", class: "quota", scope: "account", noEvidence: true},
		{name: "selected account", class: "quota", scope: "account", wantSecond: 1},
		{name: "cached window", class: "quota", scope: "model", cached: true},
		{name: "calibration refusal", class: "quota", scope: "account", calibrate: true, wantSecond: 2},
		{name: "calibration budget exhausted", class: "quota", scope: "account", calibrate: true, noRetries: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var first, second atomic.Int32
			id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				var input struct{ Account string }
				json.NewDecoder(request.Body).Decode(&input)
				if input.Account == "bob" {
					second.Add(1)
					io.WriteString(writer, `{}`)
					return
				}
				first.Add(1)
				var windows []any
				if scenario.cached {
					windows = []any{map[string]any{"id": "quota", "models": []string{"free"}, "resetAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}}
				}
				refusal := policyFixtureRefusal(scenario.class, scenario.scope, windows)
				if scenario.noEvidence {
					delete(refusal["refusal"].(map[string]any), "evidence")
				}
				json.NewEncoder(writer).Encode(refusal)
			}, func(manifest *sdk.Manifest) {
				manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ModelDiscovery)
				descriptor := &manifest.Providers[0]
				descriptor.Operations = []sdk.ProviderOperation{"systemone"}
				descriptor.Models = []sdk.Model{{ID: "free", Kind: "decision", Operations: descriptor.Operations}}
			})
			if scenario.cached {
				if err := extensionhost.Default().Store().DeleteAccount("fixture.policy", "vendor", "bob", 1); err != nil {
					t.Fatal(err)
				}
				provider.Changed()
			}
			server := New()
			t.Cleanup(server.idle.stop)
			intents := []string{"safe"}
			if scenario.calibrate {
				intents = append(intents, "unsafe")
			}
			if scenario.noRetries {
				zero := 0
				current := settings.Load()
				current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{MaxTotalRetries: &zero}}
				if err := settings.Save(current); err != nil {
					t.Fatal(err)
				}
			}
			classify := func() error {
				_, err := server.askClassifier(id+"/free", intents, before{}, false, "route this request")
				return err
			}
			err := classify()
			if (err == nil) != (scenario.wantSecond > 0) || first.Load() != 1 || second.Load() != scenario.wantSecond {
				t.Fatalf("classifier failure=%v, sends=(%d,%d)", err, first.Load(), second.Load())
			}
			observed := usage.UpstreamFailure{Status: http.StatusForbidden, Code: "denied"}
			records := decisionUsageRecords(t)
			wantCount := 1 + int(scenario.wantSecond)
			if len(records) != wantCount || records[0].Status != http.StatusForbidden || records[0].Input != 0 ||
				records[0].Rejected || records[0].UpstreamFailure != observed {
				t.Fatalf("fresh refusal lost its ledger entry: %+v", records)
			}
			calls := server.Recent()
			if len(calls) != wantCount || calls[len(calls)-1].UpstreamFailure != observed {
				t.Fatalf("fresh refusal lost its upstream observation: %+v", calls)
			}
			if scenario.cached {
				if err := classify(); err == nil || first.Load() != 1 || second.Load() != 0 {
					t.Fatalf("cached refusal sent another request: err=%v sends=(%d,%d)", err, first.Load(), second.Load())
				}
				records := decisionUsageRecords(t)
				if len(records) != 2 || !records[1].Rejected || records[1].Status != http.StatusForbidden ||
					records[1].UpstreamFailure != (usage.UpstreamFailure{}) || len(server.Recent()) != 1 {
					t.Fatalf("cached refusal recorded as an upstream send: %+v", records)
				}
			}
		})
	}
}
