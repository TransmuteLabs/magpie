package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const failureSnapshotWorker = `package main
import("context";"encoding/json";"net/http";"os";"strconv";sdk "github.com/yetone/magpie/pkg/extension")
func main(){
 raw,_:=os.ReadFile(sdk.ManifestFile);var manifest sdk.Manifest;json.Unmarshal(raw,&manifest)
 module:=sdk.Module{Manifest:manifest,Handlers:map[string]sdk.Handler{
 sdk.MethodProviderExecute:func(ctx context.Context,call sdk.Call,stream *sdk.Stream)(any,error){
  trace,err:=os.OpenFile(EXECUTION_TRACE,os.O_CREATE|os.O_APPEND|os.O_WRONLY,0600);if err!=nil{return nil,err};trace.WriteString("execute\n");trace.Close()
  var request sdk.ExecuteRequest;if err:=json.Unmarshal(call.Params,&request);err!=nil{return nil,err}
  var body struct{Messages []struct{Content string}};json.Unmarshal(request.Body,&body)
  status,_:=strconv.Atoi(body.Messages[0].Content)
  message:="PRIVATE_VENDOR_MESSAGE Bearer PRIVATE_TOKEN"
  if body.Messages[0].Content=="format" {status=400;message="response_format json_schema unsupported "+message}
  if body.Messages[0].Content=="body-cap" {status=400;message="request body too large "+message}
  if err:=stream.Emit(ctx,"accepted",sdk.AcceptedEvent{OperationID:call.Scope.Operation});err!=nil{return nil,err}
  if err:=stream.Emit(ctx,"headers",sdk.HeadersEvent{Status:status,Headers:http.Header{"Content-Type":{"application/json"}}});err!=nil{return nil,err}
  response,_:=json.Marshal(map[string]any{"error":map[string]any{"type":"vendor_permission","code":"vendor_"+strconv.Itoa(status),"message":message}})
  if err:=stream.Emit(ctx,"data",response);err!=nil{return nil,err}
  return sdk.ExecuteResult{Status:status,Outcome:"completed",Usage:&sdk.Usage{InputTokens:7,OutputTokens:3}},nil
 }}}
 if err:=sdk.Serve(context.Background(),os.Stdin,os.Stdout,module);err!=nil{panic(err)}
}`

func TestExtensionTerminalKeepsSeparateUpstreamFailure(t *testing.T) {
	directory := t.TempDir()
	manifest := extensionGatewayManifest()
	manifest.ID = "fixture.failure-snapshot"
	executionTrace := filepath.Join(t.TempDir(), "executions.log")
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(strings.ReplaceAll(failureSnapshotWorker, "EXECUTION_TRACE", strconv.Quote(executionTrace))), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o", filepath.Join(directory, manifest.Entry), source)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		status  int
		path    string
		bodies  usage.Bodies
		trigger string
	}{
		{403, "/v1/chat/completions", usage.NoBodies, ""},
		{429, "/v1/chat/completions", usage.NoBodies, ""},
		{502, "/v1/chat/completions", usage.NoBodies, ""},
		{429, "/v1/messages", usage.CapturedBodies, ""},
		{403, "/v1/messages", usage.WholeBodies, ""},
		{502, "/v1/chat/completions", usage.WholeBodies, ""},
		{400, "/v1/chat/completions", usage.CapturedBodies, "format"},
		{400, "/v1/chat/completions", usage.WholeBodies, "body-cap"},
	} {
		status := scenario.status
		t.Run(strconv.Itoa(status)+scenario.path+strconv.Itoa(int(scenario.bodies))+scenario.trigger, func(t *testing.T) {
			fresh(t)
			if err := os.WriteFile(executionTrace, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			t.Setenv("MAGPIE_ADDR", "")
			manager := extensionhost.Default()
			t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
			if _, err := manager.Install(t.Context(), directory); err != nil {
				t.Fatal(err)
			}
			provider.Changed()
			var dispatches, fallbacks atomic.Int32
			original := provider.SendsThroughPlugin
			provider.SendsThroughPlugin = func(selected string, request *http.Request) func(*http.Response, error) {
				done := original(selected, request)
				if selected == provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID) {
					dispatches.Add(1)
				}
				return done
			}
			t.Cleanup(func() { provider.SendsThroughPlugin = original })
			vendor := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbacks.Add(1)
				http.Error(w, "unexpected fallback", 500)
			})))
			t.Cleanup(vendor.Close)
			probeFixtureRoot(t, vendor)
			if err := provider.Save(provider.Provider{ID: "fallback", Name: "Fallback", Chat: vendor.URL + "/v1", Key: "fixture", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			id := provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
			if err := provider.SaveGroup(provider.Group{ID: "observability", Name: "Observability", Routing: "ordered", Members: []string{id + "/m1", "fallback/m1"}}); err != nil {
				t.Fatal(err)
			}
			observer := &fakeObserver{watch: true, bodies: scenario.bodies}
			t.Cleanup(usage.Observe(observer))
			server := New()
			t.Cleanup(server.idle.stop)
			content, format := strconv.Itoa(status), ""
			if scenario.trigger != "" {
				content = scenario.trigger
			}
			if scenario.trigger == "format" {
				format = `,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{}}}}`
			}
			request := httptest.NewRequest(http.MethodPost, scenario.path, strings.NewReader(`{"model":"group/observability","max_tokens":32`+format+`,"messages":[{"role":"user","content":"`+content+`"}]}`))
			request.RemoteAddr = "127.0.0.1:12345"
			reply := httptest.NewRecorder()
			server.Front().ServeHTTP(reply, request)
			if reply.Code != 409 || reply.Header().Get("X-Should-Retry") != "false" || reply.Header().Get("X-Magpie-Operation") == "" || fallbacks.Load() != 0 {
				t.Fatalf("no-replay boundary changed: client=%d dispatches=%d fallbacks=%d", reply.Code, dispatches.Load(), fallbacks.Load())
			}
			executions, err := os.ReadFile(executionTrace)
			if err != nil || string(executions) != "execute\n" {
				t.Fatalf("paid executions=%q error=%v", executions, err)
			}
			t.Logf("client=%d transport-attempts=%d paid-executions=1 fallback=%d", reply.Code, dispatches.Load(), fallbacks.Load())
			rows := usage.Load(time.Time{})
			calls := server.Recent()
			if len(rows) != 1 || len(calls) != 1 || rows[0].Input != 7 || rows[0].Output != 3 || rows[0].Status != 409 || rows[0].ErrType != "extension_operation_terminal" {
				t.Fatalf("terminal accounting changed: rows=%+v calls=%d", rows, len(calls))
			}
			check := func(name string, value any) {
				t.Helper()
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var projection struct {
					Failure struct {
						Status     int
						Code, Type string
					} `json:"upstream_failure"`
				}
				if json.Unmarshal(encoded, &projection) != nil {
					t.Fatal("invalid projection")
				}
				if projection.Failure.Status != status || projection.Failure.Code != "vendor_"+strconv.Itoa(status) || projection.Failure.Type != "vendor_permission" {
					t.Errorf("%s lost upstream status/code/type: %+v", name, projection.Failure)
				}
				if strings.Contains(string(encoded), "PRIVATE_") {
					t.Errorf("%s leaked raw vendor message/credential", name)
				}
			}
			check("recent", calls[0])
			check("ledger", rows[0])
			server.trace.mu.Lock()
			if len(server.trace.routes) != 1 || len(server.trace.routes[0].Tries) != 1 {
				t.Error("unexpected route/try count")
			} else {
				check("route", server.trace.routes[0])
				check("try", server.trace.routes[0].Tries[0])
			}
			server.trace.mu.Unlock()
			observer.mu.Lock()
			if len(observer.rows) != 1 || len(observer.attempts) != 1 || len(observer.ends) != 1 {
				t.Error("observer duplicated/lost call")
			} else {
				check("ledger observer", observer.rows[0])
				check("attempt observer", observer.attempts[0])
				check("end observer", observer.ends[0])
				if scenario.bodies != usage.NoBodies {
					for name, record := range map[string]usage.Record{"attempt": observer.attempts[0], "ledger": observer.rows[0]} {
						if !strings.Contains(record.BodyOut, "automatic replay stopped") || strings.Contains(record.BodyOut, "PRIVATE_") {
							t.Errorf("%s observer body is not the private-free client fence: %q", name, record.BodyOut)
						}
					}
				}
			}
			observer.mu.Unlock()
			line, ok := failedCallLine(calls[0], time.Now().Add(2*time.Minute))
			if !ok || !strings.Contains(line, "upstream_status="+strconv.Itoa(status)) || !strings.Contains(line, "upstream_code=vendor_"+strconv.Itoa(status)) || !strings.Contains(line, "upstream_type=vendor_permission") {
				t.Error("failed-call diagnostic lost separate upstream facts")
			}
			if strings.Contains(line+reply.Body.String(), "PRIVATE_") {
				t.Error("diagnostic/client leaked raw vendor message/credential")
			}
		})
	}
}
