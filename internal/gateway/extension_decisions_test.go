package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

var sdk17NativeChild = flag.Bool("sdk17-native-child", false, "run the SDK 1.7 native decision fixture in this test process")

type decisionTransport func(*http.Request) (*http.Response, error)

func (transport decisionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func decisionFixtureManifest(test *testing.T, kind string) sdk.Manifest {
	test.Helper()
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(`{"schema":1,"id":"fixture.decision","version":"1.0.0","runtime":"exec","entry":"worker",
		"compatibility":{"hostAPI":"^1.7"},
		"contract":{"version":{"major":1,"minor":7},"capabilities":["outbound-executor","model-discovery","account-auth"]},
		"providers":[{"id":"judge","name":"Judge","protocols":["chat"],"operations":["systemone"],
		"models":[{"id":"decision","kind":"decision","operations":["systemone"]},{"id":"conversation","kind":"chat"}]}]}`),
		&manifest); err != nil {
		test.Fatal(err)
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
	return manifest
}

func buildDecisionFixture(test *testing.T, kind string) string {
	test.Helper()
	directory := test.TempDir()
	manifest := decisionFixtureManifest(test, kind)
	arguments := []string{"build", "-trimpath", "-o", filepath.Join(directory, manifest.Entry)}
	if kind == "native" {
		arguments = append(arguments, "-buildmode=c-shared")
	}
	arguments = append(arguments, "./testdata/extensiondecisionworker")
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), arguments...)
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=1")
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("build %s decision fixture: %v\n%s", kind, err, output)
	}
	body, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
		test.Fatal(err)
	}
	return directory
}

func installDecisionFixture(test *testing.T, directory, mode string) (*extensionhost.Manager, string) {
	test.Helper()
	fresh(test)
	test.Setenv("MAGPIE_NO_ACCOUNTS", "")
	test.Setenv("MAGPIE_ADDR", "")
	manager := extensionhost.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
	for _, account := range []string{"alice", "bob"} {
		auth, _ := json.Marshal(map[string]string{"access": "fixture-" + account, "mode": mode})
		if account == "bob" {
			auth = json.RawMessage(`{"access":"fixture-bob"}`)
		}
		if _, err := manager.Store().SaveAccount(extensionhost.Account{
			Plugin: "fixture.decision", Provider: "judge", ID: account, Auth: auth,
		}, 0); err != nil {
			test.Fatal(err)
		}
	}
	provider.Changed()
	accounts, err := provider.ExtensionProviders()
	if err != nil {
		test.Fatal(err)
	}
	for _, account := range accounts {
		if _, err := account.Fetch(test.Context()); err != nil {
			test.Fatal(err)
		}
	}
	provider.Changed()
	return manager, provider.ExtensionProviderID("fixture.decision", "judge")
}

func decisionExecutions(test *testing.T, manager *extensionhost.Manager, account string) []struct {
	Operation string             `json:"operation"`
	Account   string             `json:"account"`
	Request   sdk.ExecuteRequest `json:"request"`
} {
	test.Helper()
	selected, err := manager.Store().GetAccount("fixture.decision", "judge", account)
	if err != nil {
		test.Fatal(err)
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: "fixture.decision", Provider: "judge", Account: account,
		Generation: selected.Generation}, "executions")
	if err != nil {
		test.Fatal(err)
	}
	var entries []struct {
		Operation string             `json:"operation"`
		Account   string             `json:"account"`
		Request   sdk.ExecuteRequest `json:"request"`
	}
	if len(state.Value) > 0 {
		if err := json.Unmarshal(state.Value, &entries); err != nil {
			test.Fatal(err)
		}
	}
	return entries
}

func decisionUsageRecords(test *testing.T) []usage.Record {
	test.Helper()
	body, err := os.ReadFile(usage.Path())
	if err != nil {
		test.Fatal(err)
	}
	var records []usage.Record
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var record usage.Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			test.Fatal(err)
		}
		if strings.HasPrefix(record.Provider, "extension-") {
			records = append(records, record)
		}
	}
	return records
}

func TestExtensionDecisionsSDK17(test *testing.T) {
	for _, kind := range []string{"exec", "native"} {
		test.Run(kind, func(test *testing.T) {
			if kind == "native" && !*sdk17NativeChild {
				ctx, cancel := context.WithTimeout(test.Context(), 3*time.Minute)
				defer cancel()
				command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestExtensionDecisionsSDK17$/^native$",
					"-test.v", "-test.timeout=150s", "-sdk17-native-child")
				output, err := command.CombinedOutput()
				if err != nil {
					test.Fatalf("isolated native decision fixture: %v\n%s", err, output)
				}
				test.Log(string(output))
				return
			}
			directory := buildDecisionFixture(test, kind)
			test.Run("ordinary SystemOne and Decisions", func(test *testing.T) {
				manager, id := installDecisionFixture(test, directory, "")
				observer := &fakeObserver{}
				test.Cleanup(usage.Observe(observer))
				server := New()
				test.Cleanup(server.idle.stop)
				for _, path := range []string{"/v1/systemone", "/v1/decisions"} {
					body := `{"model":"` + id + `/decision","state":{"message":"hello"},"questions":{"safe":{"type":"noul"}}}`
					if path == "/v1/decisions" {
						body = `{"model":"` + id + `/decision","input":"hello","questions":[{"name":"safe","type":"predicate","instructions":"safe?"}]}`
					}
					reply := mediaFixtureCall(server.Front(), http.MethodPost, path, body)
					if reply.Code != 200 || !strings.Contains(reply.Body.String(), `"answers"`) ||
						reply.Header().Get("X-Request-Id") == "" || reply.Header().Get("Set-Cookie") != "" ||
						reply.Header().Get("X-Magpie-Operation") == "forged" {
						test.Fatalf("ordinary decision response: %d %v %s", reply.Code, reply.Header(), reply.Body)
					}
				}
				entries := decisionExecutions(test, manager, "alice")
				if len(entries) != 2 || entries[0].Operation == entries[1].Operation || len(decisionExecutions(test, manager, "bob")) != 0 {
					test.Fatalf("ordinary decisions lost selected operations: %+v", entries)
				}
				records := decisionUsageRecords(test)
				if len(records) != 2 {
					test.Fatalf("decision accounting appended %d records: %+v", len(records), records)
				}
				for _, record := range records {
					if record.Input != 41 || record.Output != 3 || record.CacheRead != 5 || record.CacheWrite != 2 {
						test.Fatalf("normalized decision snapshot was erased or counted twice: %+v", record)
					}
				}
				observer.mu.Lock()
				observed := append([]usage.Record(nil), observer.rows...)
				observer.mu.Unlock()
				if len(observed) != 2 || len(server.Recent()) != 2 || len(server.trace.routes) != 2 {
					test.Fatalf("ordinary decision observer/log/route mismatch: rows=%d calls=%d routes=%d",
						len(observed), len(server.Recent()), len(server.trace.routes))
				}
				for _, row := range observed {
					if row.Input != 41 || row.Output != 3 || row.CacheRead != 5 || row.CacheWrite != 2 || row.Status != 200 {
						test.Fatalf("observer lost the normalized decision snapshot: %+v", row)
					}
				}
				request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set(provider.DecidersHeader, "true")
				reply := httptest.NewRecorder()
				server.Front().ServeHTTP(reply, request)
				if reply.Code != 200 || !strings.Contains(reply.Body.String(), `"/v1/systemone"`) {
					test.Fatalf("SDK decision missing native model endpoint: %d %s", reply.Code, reply.Body)
				}
				reply = mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
					`{"model":"`+id+`/decision","messages":[{"role":"user","content":"hello"}]}`)
				if reply.Code != 400 || len(decisionExecutions(test, manager, "alice")) != 2 {
					test.Fatalf("decision model became chat: %d %s", reply.Code, reply.Body)
				}
			})
			test.Run("display union never authorizes an empty primary account", func(test *testing.T) {
				manager, id := installDecisionFixture(test, directory, "empty")
				if err := provider.SaveGroup(provider.Group{ID: "secondary", Name: "Secondary",
					Members: []string{id + "/decision"}}); err != nil {
					test.Fatal(err)
				}
				server := New()
				test.Cleanup(server.idle.stop)
				for _, asked := range []string{id + "/decision", "group/secondary"} {
					reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/systemone",
						`{"model":"`+asked+`","state":"hello","questions":{"safe":{"type":"noul"}}}`)
					if reply.Code != 200 || len(decisionExecutions(test, manager, "alice")) != 0 {
						test.Fatalf("display union lost the secondary or authorized the empty primary: %d %s", reply.Code, reply.Body)
					}
				}
				if len(decisionExecutions(test, manager, "bob")) != 2 {
					test.Fatal("decision requests did not use the actual entitled account")
				}
			})
			for _, mode := range []string{"accepted 503", "accepted error", "partial", "uncertain", "not-started"} {
				test.Run("group/"+mode, func(test *testing.T) {
					manager, id := installDecisionFixture(test, directory, mode)
					if err := provider.SaveGroup(provider.Group{ID: "judges", Name: "Judges",
						Routing: provider.Ordered, Members: []string{id + "/decision"}}); err != nil {
						test.Fatal(err)
					}
					server := New()
					test.Cleanup(server.idle.stop)
					reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/systemone",
						`{"model":"group/judges","state":"hello","questions":{"safe":{"type":"noul"}}}`)
					entries := decisionExecutions(test, manager, "bob")
					if mode == "not-started" {
						if reply.Code != 200 || len(entries) != 1 {
							test.Fatalf("safe decision fallback failed: %d %s bob=%d", reply.Code, reply.Body, len(entries))
						}
					} else if reply.Code != 409 || reply.Header().Get("X-Should-Retry") != "false" ||
						reply.Header().Get("X-Magpie-Operation") == "" || len(entries) != 0 {
						test.Fatalf("terminal decision replayed: %d %v %s bob=%d", reply.Code, reply.Header(), reply.Body, len(entries))
					}
					records := decisionUsageRecords(test)
					if len(records) != 1 || records[0].ProviderAccount != "alice" && mode != "not-started" {
						test.Fatalf("decision retry accounting lost its execution owner: %+v", records)
					}
					if mode != "not-started" && (records[0].Status != 409 || records[0].Error == "" || records[0].CacheRead != 5 ||
						len(server.Recent()) != 1 || server.Recent()[0].Status != 409 ||
						len(server.trace.routes) != 1 || !server.trace.routes[0].Done || len(server.trace.routes[0].Tries) != 1) {
						test.Fatalf("terminal decision vanished from ledger/log/route: rows=%+v calls=%+v", records, server.Recent())
					}
				})
			}
			for _, mode := range []string{"accepted error", "accepted 503", "invalid answer", "nested accepted error", "deadline"} {
				test.Run("original classifier turn/"+mode, func(test *testing.T) {
					manager, id := installDecisionFixture(test, directory, strings.TrimPrefix(mode, "nested "))
					if mode == "deadline" {
						previous := classifyUnit
						classifyUnit = 250 * time.Millisecond
						test.Cleanup(func() { classifyUnit = previous })
					}
					var downstream atomic.Int32
					backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						payload, err := io.ReadAll(request.Body)
						if err != nil {
							http.Error(writer, err.Error(), http.StatusBadRequest)
							return
						}
						if strings.Contains(string(payload), "fix this bug") || strings.Contains(string(payload), "fix another bug") {
							downstream.Add(1)
						}
						if request.Method == http.MethodGet {
							io.WriteString(writer, `{"data":[{"id":"conversation"}]}`)
							return
						}
						io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"default"}}]}`)
					}))
					test.Cleanup(backend.Close)
					if err := provider.Save(provider.Provider{ID: "target", Name: "Target", Chat: backend.URL + "/v1",
						Key: "fixture", Models: []string{"conversation"}}); err != nil {
						test.Fatal(err)
					}
					if err := provider.SaveGroup(provider.Group{ID: "route", Name: "Route", Members: []string{"target/conversation"},
						Classifier: id + "/decision", Rules: []provider.Rule{{Use: "target/conversation", Intent: "bug"}}}); err != nil {
						test.Fatal(err)
					}
					asked := "group/route"
					if strings.HasPrefix(mode, "nested ") {
						if err := provider.SaveGroup(provider.Group{ID: "outer", Name: "Outer", Members: []string{"group/route"}}); err != nil {
							test.Fatal(err)
						}
						asked = "group/outer"
					}
					server := New()
					test.Cleanup(server.idle.stop)
					reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
						`{"model":"`+asked+`","messages":[{"role":"user","content":"fix this bug"}]}`)
					if reply.Code != 409 || reply.Header().Get("X-Should-Retry") != "false" ||
						reply.Header().Get("X-Magpie-Operation") == "" || downstream.Load() != 0 ||
						len(decisionExecutions(test, manager, "bob")) != 0 {
						test.Fatalf("terminal classifier fell through to another execution: %d %v %s downstream=%d",
							reply.Code, reply.Header(), reply.Body, downstream.Load())
					}
					records := decisionUsageRecords(test)
					if len(records) != 1 || records[0].Status != 409 || records[0].Error == "" {
						test.Fatalf("classifier terminal execution lost its ledger status: %+v", records)
					}
					if mode != "invalid answer" {
						cached := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
							`{"model":"`+asked+`","messages":[{"role":"user","content":"fix another bug"}]}`)
						if cached.Code != 409 || cached.Header().Get("X-Should-Retry") != "false" ||
							cached.Header().Get("X-Magpie-Operation") != "" || downstream.Load() != 0 ||
							len(decisionExecutions(test, manager, "alice")) != 1 || len(decisionUsageRecords(test)) != 1 {
							test.Fatalf("cached classifier failure lost its fence or leaked a receipt: %d %v %s",
								cached.Code, cached.Header(), cached.Body)
						}
					}
				})
			}
			test.Run("calibration and main have independent usage and receipts", func(test *testing.T) {
				manager, id := installDecisionFixture(test, directory, "main nil usage")
				server := New()
				test.Cleanup(server.idle.stop)
				selected, err := provider.Find(id)
				if err != nil {
					test.Fatal(err)
				}
				ctx := extensionhost.WithRequestMetadata(test.Context(), extensionhost.RequestMetadata{
					Plugin: "fixture.decision", RequestID: "one-classifier-sequence", CallerID: "caller"})
				if _, err := server.askJev(ctx, 10*time.Second, *selected, "decision", []string{"feature", "bug"}, before{}, false, "fix this"); err != nil {
					test.Fatal(err)
				}
				entries := decisionExecutions(test, manager, "alice")
				if len(entries) != 2 || entries[0].Operation == entries[1].Operation {
					test.Fatalf("classifier reused execution receipts: %+v", entries)
				}
				records := decisionUsageRecords(test)
				if len(records) != 2 || records[0].Input != 11 || records[0].Output != 2 ||
					records[1].Input != 41 || records[1].Output != 3 {
					test.Fatalf("calibration usage leaked into the main call: %+v", records)
				}
				if len(server.Recent()) != 2 || server.Recent()[0].Usage.Input != 41 || server.Recent()[1].Usage.Input != 11 {
					test.Fatalf("classifier execution log shared usage: %+v", server.Recent())
				}
			})
			test.Run("calibration completion does not fence a not-started main", func(test *testing.T) {
				manager, id := installDecisionFixture(test, directory, "main not-started")
				server := New()
				test.Cleanup(server.idle.stop)
				if _, err := server.askClassifier(id+"/decision", []string{"feature", "bug"}, before{}, false, "fix this"); err != nil {
					test.Fatal(err)
				}
				alice, bob := decisionExecutions(test, manager, "alice"), decisionExecutions(test, manager, "bob")
				if len(alice) != 2 || len(bob) != 1 {
					test.Fatalf("classifier replayed calibration or fenced an unstarted main: alice=%+v bob=%+v", alice, bob)
				}
				if records := decisionUsageRecords(test); len(records) != 2 || records[0].Input != 11 || records[1].Input != 41 {
					test.Fatalf("classifier reused calibration totals after safe fallback: %+v", records)
				}
			})
			test.Run("event usage survives absent final usage", func(test *testing.T) {
				_, id := installDecisionFixture(test, directory, "event only")
				server := New()
				test.Cleanup(server.idle.stop)
				reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/systemone",
					`{"model":"`+id+`/decision","state":"hello","questions":{"safe":{"type":"noul"}}}`)
				records := decisionUsageRecords(test)
				if reply.Code != 200 || len(records) != 1 || records[0].Input != 40 || records[0].Output != 3 {
					test.Fatalf("nil final usage erased the event snapshot: %d %+v", reply.Code, records)
				}
			})
			test.Run("present zero result overrides event and raw usage", func(test *testing.T) {
				_, id := installDecisionFixture(test, directory, "zero result")
				server := New()
				test.Cleanup(server.idle.stop)
				reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/systemone",
					`{"model":"`+id+`/decision","state":"hello","questions":{"safe":{"type":"noul"}}}`)
				records := decisionUsageRecords(test)
				if reply.Code != 200 || len(records) != 1 || records[0].Input != 0 || records[0].Output != 0 {
					test.Fatalf("present zero result borrowed another usage snapshot: %d %+v", reply.Code, records)
				}
			})
			for _, mode := range []string{"rich usage", "rich failure", "rich nonUSD", "rich event only", "rich empty final"} {
				test.Run(mode, func(test *testing.T) {
					manager, id := installDecisionFixture(test, directory, mode)
					observer := &fakeObserver{}
					test.Cleanup(usage.Observe(observer))
					server := New()
					test.Cleanup(server.idle.stop)
					reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/systemone",
						`{"model":"`+id+`/decision","state":"hello","questions":{"safe":{"type":"noul"}}}`)
					status, input, output := 200, 4, 4
					if mode == "rich failure" {
						status = 409
					}
					if mode == "rich event only" {
						input, output = 1003, 1000
					}
					if mode == "rich empty final" {
						input, output = 41, 3
					}
					records := decisionUsageRecords(test)
					if reply.Code != status || len(records) != 1 || len(decisionExecutions(test, manager, "bob")) != 0 {
						test.Fatalf("rich decision lost its execution owner: %d %s rows=%+v", reply.Code, reply.Body, records)
					}
					record := records[0]
					if record.Input != input || record.Output != output || record.Status != status || record.ResponseID != "" {
						test.Fatalf("sparse final usage borrowed an event or erased raw counts: %+v", record)
					}
					observer.mu.Lock()
					observed := append([]usage.Record(nil), observer.rows...)
					observer.mu.Unlock()
					if len(observed) != 1 || observed[0].Input != input || observed[0].Output != output || observed[0].Status != status ||
						observed[0].ResponseID != "" ||
						len(server.Recent()) != 1 || server.Recent()[0].Status != status ||
						server.Recent()[0].Usage.ResponseID != "" ||
						len(server.trace.routes) != 1 || !server.trace.routes[0].Done || server.trace.routes[0].Status != status ||
						server.trace.routes[0].ResponseID != "" {
						test.Fatalf("rich decision lost an accounting projection: observed=%+v calls=%+v routes=%+v",
							observed, server.Recent(), server.trace.routes)
					}
					if mode == "rich empty final" {
						if record.Credits != nil || record.ReportedCost != nil || record.OwnCost != nil ||
							record.Served != "decision" || record.CacheWrite1h != 0 || record.Reasoning != 0 ||
							record.UpstreamResponseID != "" || observed[0].UpstreamResponseID != "" ||
							server.Recent()[0].Usage.UpstreamResponseID != "" || server.Recent()[0].Usage.Deferred != nil {
							test.Fatalf("empty final facts retained a previous event snapshot: %+v", record)
						}
						return
					}
					currency := "USD"
					if mode == "rich nonUSD" {
						currency = "CNY"
					}
					if record.CacheRead != 7 || record.CacheWrite != 7 || record.CacheWrite1h != 3 || record.Reasoning != 3 ||
						record.Served != "served-decision" || record.Upstream != "fixture-upstream" ||
						record.RequestID != "upstream-request" || record.UpstreamResponseID != "decision-response" ||
						record.Stop != "answer" || record.TierServed != "priority" ||
						record.Credits == nil || *record.Credits != 0.125 ||
						record.ReportedCost == nil || record.ReportedCost.Amount != 0.05 || record.ReportedCost.Currency != currency {
						test.Fatalf("decision ledger dropped rich execution facts: %+v", record)
					}
					if currency == "USD" && (record.OwnCost == nil || *record.OwnCost != 0.05) ||
						currency != "USD" && record.OwnCost != nil {
						test.Fatalf("decision accounting invented a currency conversion: %+v", record)
					}
					if status == 409 && (record.ErrType != "fixture_terminal" || reply.Header().Get("X-Should-Retry") != "false") {
						test.Fatalf("decision failure lost its typed accounting or replay fence: %+v %v", record, reply.Header())
					}
					recent := server.Recent()[0].Usage
					routed := server.trace.routes[0].Usage
					if recent.Deferred == nil || recent.CacheWrite1h != 2 || recent.Reasoning != 2 ||
						recent.ReportedCost == nil || recent.ReportedCost.Currency != currency ||
						recent.UpstreamResponseID != "decision-response" || observed[0].UpstreamResponseID != "decision-response" ||
						observed[0].CacheWrite1h != 3 || observed[0].Reasoning != 3 || observed[0].ReportedCost == nil ||
						len(routed) != 2 || routed[0].Input != 4 || routed[1].CacheWrite1h != 2 ||
						routed[1].ReportedCost == nil || routed[1].ReportedCost.Currency != currency {
						test.Fatalf("decision observer/recent/route dropped rich usage: observed=%+v recent=%+v routed=%+v",
							observed, recent, routed)
					}
				})
			}
			test.Run("translation failure after completion is terminal", func(test *testing.T) {
				manager, id := installDecisionFixture(test, directory, "malformed answer")
				server := New()
				test.Cleanup(server.idle.stop)
				reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/decisions",
					`{"model":"`+id+`/decision","input":"hello","questions":[{"name":"safe","type":"predicate","instructions":"safe?"}]}`)
				if reply.Code != 409 || reply.Header().Get("X-Should-Retry") != "false" ||
					reply.Header().Get("X-Magpie-Operation") == "" || len(decisionExecutions(test, manager, "alice")) != 1 ||
					len(decisionExecutions(test, manager, "bob")) != 0 || len(decisionUsageRecords(test)) != 1 {
					test.Fatalf("completed untranslatable decision became replayable: %d %v %s", reply.Code, reply.Header(), reply.Body)
				}
			})
		})
	}
}

type decisionReadFailure struct {
	io.Reader
	readErr  error
	closeErr error
}

func (body *decisionReadFailure) Read(data []byte) (int, error) {
	count, err := body.Reader.Read(data)
	if err == io.EOF && body.readErr != nil {
		return count, body.readErr
	}
	return count, err
}

func (body *decisionReadFailure) Close() error { return body.closeErr }

func TestDecisionReplyRetainsTypedTransportFailure(test *testing.T) {
	fresh(test)
	refused := &sdk.Error{Code: "fixture_failed", Message: "accepted decision failed", Outcome: "accepted"}
	server := New()
	test.Cleanup(server.idle.stop)
	server.client = &http.Client{Transport: decisionTransport(func(*http.Request) (*http.Response, error) {
		return nil, refused
	})}
	_, err := server.postDecideSent(context.Background(), provider.Provider{ID: "judge", Name: "Judge", Decide: "https://fixture.invalid/v1"},
		"decision", []byte(`{"model":"decision","questions":{}}`))
	var typed *sdk.Error
	if !errors.As(err, &typed) || typed != refused {
		test.Fatalf("decision owner erased typed transport outcome: %v", err)
	}
}

func TestDecisionClassifierCacheRetainsTerminalCause(test *testing.T) {
	fresh(test)
	var calls int
	ask := func(string, []string, before, bool, string) (verdict, error) {
		calls++
		return verdict{}, &sdk.Error{Code: "fixture_failed", Message: "accepted classifier failed", Outcome: "accepted"}
	}
	for _, text := range []string{"first question", "second question"} {
		_, _, err := classify(ask, test.Name(), []string{"bug"}, before{}, false, text)
		var typed *sdk.Error
		if !errors.As(err, &typed) || typed.Outcome != "accepted" {
			test.Fatalf("classifier cache erased the terminal outcome: %v", err)
		}
	}
	if calls != 1 {
		test.Fatalf("terminal classifier was automatically replayed %d times", calls)
	}
}

func TestDecisionClassifierSharedFailureKeepsFenceWithoutReceipt(test *testing.T) {
	fresh(test)
	started, release := make(chan struct{}), make(chan struct{})
	ask := func(string, []string, before, bool, string) (verdict, error) {
		close(started)
		<-release
		return verdict{}, &decisionOperationError{
			cause:  &sdk.Error{Code: "fixture_failed", Message: "accepted decision failed", Outcome: "accepted"},
			module: "fixture.decision", operation: "caller-one"}
	}
	owner, waiter := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := classify(ask, test.Name(), []string{"bug"}, before{}, false, "shared question")
		owner <- err
	}()
	<-started
	go func() {
		_, _, err := classify(ask, test.Name(), []string{"bug"}, before{}, false, "shared question")
		waiter <- err
	}()
	close(release)
	var receipt *decisionOperationError
	if err := <-owner; !errors.As(err, &receipt) || receipt.operation != "caller-one" {
		test.Fatalf("operation owner lost its receipt: %v", err)
	}
	for _, err := range []error{<-waiter, func() error {
		_, _, err := classify(ask, test.Name(), []string{"bug"}, before{}, false, "different question")
		return err
	}()} {
		var shared *decisionOperationError
		if !terminalExtensionError(err) || !errors.As(err, &shared) || shared.operation != "" || shared.module != "" {
			test.Fatalf("shared classifier result lost fence or leaked another receipt: %v %+v", err, shared)
		}
	}
}

func TestDecisionClassifierDeadlineRetainsTerminalCause(test *testing.T) {
	fresh(test)
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	server := New()
	test.Cleanup(server.idle.stop)
	server.client = &http.Client{Transport: decisionTransport(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, &sdk.Error{Code: "fixture_failed", Message: "accepted classifier expired", Outcome: "uncertain"}
	})}
	_, err := server.systemOne(ctx, provider.Provider{ID: "judge", Name: "Judge", Decide: "https://fixture.invalid/v1"},
		"decision", []byte(`{"model":"decision","questions":{}}`))
	var typed *sdk.Error
	if !errors.As(err, &typed) || typed.Outcome != "uncertain" {
		test.Fatalf("classifier deadline erased an uncertain operation: %v", err)
	}
}

func TestDecisionReplyRequiresCompleteBoundedBody(test *testing.T) {
	for _, scenario := range []struct {
		name, body string
		readErr    error
		closeErr   error
	}{
		{name: "partial read", body: `{"answers":{}}`, readErr: io.ErrUnexpectedEOF},
		{name: "failed close", body: `{"answers":{}}`, closeErr: io.ErrClosedPipe},
		{name: "oversize", body: strings.Repeat(" ", (1<<20)+1)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fresh(test)
			server := New()
			test.Cleanup(server.idle.stop)
			server.client = &http.Client{Transport: decisionTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: &decisionReadFailure{Reader: strings.NewReader(scenario.body), readErr: scenario.readErr, closeErr: scenario.closeErr}}, nil
			})}
			_, err := server.postDecideSent(context.Background(), provider.Provider{ID: "judge", Name: "Judge", Decide: "https://fixture.invalid/v1"},
				"decision", []byte(`{"model":"decision","questions":{}}`))
			if err == nil {
				test.Fatal("incomplete decision response became a successful answer")
			}
			if scenario.readErr != nil && !errors.Is(err, scenario.readErr) || scenario.closeErr != nil && !errors.Is(err, scenario.closeErr) {
				test.Fatalf("decision read/close error lost its cause: %v", err)
			}
		})
	}
}
