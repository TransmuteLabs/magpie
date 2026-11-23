package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionOpenCodeProjectRefusalThroughFront(t *testing.T) {
	worker, err := os.ReadFile(buildExtensionGatewayWorker(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	manifest := extensionGatewayManifest()
	manifest.ID, manifest.Providers[0].ID = "opencode", "opencode-go"
	moduleJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, path, signal, clientCode string
		group, stream, pass            bool
		retries, total, want           int
		fallbacks                      int32
	}{
		{name: "native_missing", signal: "missing", clientCode: "project_required", retries: 2, total: 2, want: 400},
		{name: "native_malformed", signal: "malformed", clientCode: "invalid_project", want: 400},
		{name: "translated_unary", path: "/v1/messages", signal: "malformed", want: 400},
		{name: "translated_stream", path: "/v1/messages", signal: "missing", stream: true, want: 400},
		{name: "eligible_sibling", signal: "missing", group: true, total: 1, want: 200, fallbacks: 1},
		{name: "shared_budget_exhausted", signal: "missing", clientCode: "project_required", group: true, want: 400},
		{name: "pass_errors_on_sibling", signal: "missing", group: true, pass: true, total: 2, want: 422, fallbacks: 1},
		{name: "accepted_fence", signal: "accepted", group: true, retries: 2, total: 2, want: 409},
		{name: "unrelated_invalid_params", signal: "unrelated", want: 502},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			t.Setenv("MAGPIE_ADDR", "")
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, manifest.Entry), worker, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), moduleJSON, 0o600); err != nil {
				t.Fatal(err)
			}
			manager := extensionhost.Default()
			t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
			if _, err := manager.Install(t.Context(), directory); err != nil {
				t.Fatal(err)
			}
			provider.Changed()
			id := provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
			var dispatches, fallbacks atomic.Int32
			original := provider.SendsThroughPlugin
			provider.SendsThroughPlugin = func(selected string, request *http.Request) func(*http.Response, error) {
				if selected == id {
					dispatches.Add(1)
				}
				return original(selected, request)
			}
			t.Cleanup(func() { provider.SendsThroughPlugin = original })
			model := id + "/m1"
			if scenario.group {
				vendor := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fallbacks.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if scenario.pass {
						w.WriteHeader(http.StatusUnprocessableEntity)
						io.WriteString(w, `{"error":{"code":"fixture_pass","message":"sibling wire error"}}`)
						return
					}
					io.WriteString(w, `{"id":"fallback","choices":[{"message":{"role":"assistant","content":"sibling answer"},"finish_reason":"stop"}]}`)
				})))
				t.Cleanup(vendor.Close)
				probeFixtureRoot(t, vendor)
				if err := provider.Save(provider.Provider{
					ID: "fallback", Name: "Fallback", Chat: vendor.URL + "/v1", Key: "fixture", Models: []string{"m1"}, PassErrors: scenario.pass,
				}); err != nil {
					t.Fatal(err)
				}
				if err := provider.SaveGroup(provider.Group{
					ID: "project-fixture", Name: "Project fixture", Routing: "ordered", Members: []string{model, "fallback/m1"},
				}); err != nil {
					t.Fatal(err)
				}
				model = "group/project-fixture"
			}
			current := settings.Load()
			zero := 0
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxRetries: &scenario.retries, MaxTotalRetries: &scenario.total, BaseDelayMS: &zero,
			}}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			server := New()
			t.Cleanup(server.idle.stop)
			body, err := json.Marshal(map[string]any{
				"model": model, "stream": scenario.stream, "max_tokens": 32,
				"messages": []map[string]string{{"role": "user", "content": "project-fixture-" + scenario.signal}},
			})
			if err != nil {
				t.Fatal(err)
			}
			path := scenario.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			request.RemoteAddr = "127.0.0.1:12345"
			reply := httptest.NewRecorder()
			server.Front().ServeHTTP(reply, request)
			if reply.Code != scenario.want || dispatches.Load() != 1 || fallbacks.Load() != scenario.fallbacks {
				t.Errorf("status=%d SDK-attempts=%d siblings=%d body=%s", reply.Code, dispatches.Load(), fallbacks.Load(), reply.Body)
			}
			if scenario.want == http.StatusBadRequest || scenario.want == http.StatusConflict {
				if reply.Header().Get("X-Should-Retry") != "false" {
					t.Errorf("missing retry refusal: %v", reply.Header())
				}
			}
			if scenario.want == http.StatusBadRequest {
				var failure struct {
					Type  string `json:"type"`
					Error struct{ Type, Code, Message string }
				}
				if err := json.Unmarshal(reply.Body.Bytes(), &failure); err != nil ||
					failure.Error.Type != "invalid_request_error" || failure.Error.Code != scenario.clientCode ||
					!strings.Contains(failure.Error.Message, "projectId") ||
					(path == "/v1/messages" && failure.Type != "error") {
					t.Errorf("incorrect local error envelope: %s (%v)", reply.Body, err)
				}
			}
			if scenario.pass && !strings.Contains(reply.Body.String(), `"code":"fixture_pass"`) {
				t.Errorf("sibling errors=pass changed the original error: %s", reply.Body)
			}
			if scenario.want == http.StatusOK && !strings.Contains(reply.Body.String(), "sibling answer") {
				t.Errorf("missing sibling response: %s", reply.Body)
			}
			if scenario.signal != "unrelated" {
				if rests := RestsUnder(id); len(rests) != 0 {
					t.Errorf("local refusal rested an SDK candidate: %+v", rests)
				}
			}
			if scenario.signal != "unrelated" && scenario.signal != "accepted" {
				code := "project_required"
				if scenario.signal == "malformed" {
					code = "invalid_project"
				}
				server.trace.mu.Lock()
				if len(server.trace.routes) != 1 || len(server.trace.routes[0].Tries) != 1+int(scenario.fallbacks) {
					t.Error("route did not record one rejected attempt and its permitted sibling")
				} else if first := server.trace.routes[0].Tries[0]; first.Status != http.StatusBadRequest ||
					first.Fail != code || first.Rest != nil || first.Again != 0 {
					t.Errorf("typed local rejection became a retried or rested upstream attempt: %+v", first)
				}
				server.trace.mu.Unlock()
			}
			operations := filepath.Join(manager.Namespace(), "private", manifest.ID, "operations")
			entries, err := os.ReadDir(operations)
			if err != nil || len(entries) != 1 {
				t.Fatalf("operation count=%d error=%v", len(entries), err)
			}
			record, err := os.ReadFile(filepath.Join(operations, entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			var entry extensionhost.JournalEntry
			if err := json.Unmarshal(record, &entry); err != nil {
				t.Fatal(err)
			}
			wantState := "not-started"
			if scenario.signal == "accepted" {
				wantState = "uncertain"
				if reply.Header().Get("X-Magpie-Operation") != entry.Scope.Operation ||
					!strings.Contains(reply.Body.String(), "automatic replay stopped") {
					t.Errorf("accepted fence hid the operation receipt: %v %s", reply.Header(), reply.Body)
				}
			}
			receipt, err := manager.Store().Operation(manifest.ID, entry.Scope.Operation)
			if err != nil || receipt.State != wantState {
				t.Errorf("durable state=%q, want %q (%v)", receipt.State, wantState, err)
			}
		})
	}
}

type projectRefusalHeadRecorder struct {
	*httptest.ResponseRecorder
	heads []int
}

func (writer *projectRefusalHeadRecorder) WriteHeader(status int) {
	writer.heads = append(writer.heads, status)
	writer.ResponseRecorder.WriteHeader(status)
}

func TestExtensionOpenCodeProjectRefusalAfterKeepalive(t *testing.T) {
	for _, protocol := range []provider.Protocol{provider.Chat, provider.Anthropic, provider.Responses} {
		t.Run(string(protocol), func(t *testing.T) {
			reply := &projectRefusalHeadRecorder{ResponseRecorder: httptest.NewRecorder()}
			kept := &keptAlive{proto: protocol, streams: true, model: "m1"}
			held := newHoldWriter(reply, false)
			held.alive = kept
			held.takeProjectRefusal = func() *sdk.Error { return &sdk.Error{Code: "project_required"} }
			held.keepAlive()
			writeError(held, protocol, http.StatusBadGateway, "synthetic bridge error")
			if held.passing || held.projectRefusal == nil {
				t.Fatal("bridge error escaped before routing handled the project refusal")
			}
			writeProjectRefusal(reply, kept, protocol, "Provide a projectId", held.projectRefusal.Code)
			body := reply.Body.String()
			if len(reply.heads) != 1 || reply.heads[0] != http.StatusOK || !strings.Contains(body, ": keepalive") ||
				strings.Count(body, `"code":"project_required"`) != 1 || strings.Contains(body, "synthetic bridge error") {
				t.Fatalf("heads=%v body=%s", reply.heads, body)
			}
		})
	}
}
