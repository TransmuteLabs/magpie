package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sdkMiddlewareTrace struct {
	Method      string             `json:"method"`
	Request     sdk.MiddlewareCall `json:"request"`
	Scope       sdk.Scope          `json:"scope"`
	HasAuth     bool               `json:"hasAuth"`
	WriteDenied bool               `json:"writeDenied"`
}

func buildSDKMiddleware(test testing.TB) string {
	test.Helper()
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/extensionmiddlewareworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("build SDK middleware fixture: %v\n%s", err, output)
	}
	return directory
}

func installSDKMiddleware(test testing.TB, manager *extension.Manager, directory, version string, descriptor sdk.GatewayMiddlewareDescriptor) {
	test.Helper()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "middleware.fixture", Version: version,
		Runtime: "exec", Entry: entry, Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.GatewayMiddleware}}, Middleware: &descriptor}
	raw, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
}

func sdkMiddlewareDescriptor() sdk.GatewayMiddlewareDescriptor {
	return sdk.GatewayMiddlewareDescriptor{Request: true, Account: true, Provider: true, Event: true, Response: true}
}

func configureSDKMiddleware(test testing.TB, manager *extension.Manager, options map[string]any) {
	test.Helper()
	config, err := manager.ModuleConfig(test.Context(), "middleware.fixture")
	if err != nil {
		test.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), "middleware.fixture", config.Version, raw); err != nil {
		test.Fatal(err)
	}
}

func setupSDKMiddleware(test *testing.T, directory string, descriptor sdk.GatewayMiddlewareDescriptor, options map[string]any) *extension.Manager {
	test.Helper()
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent-bun"))
	test.Setenv("MAGPIE_NO_ACCOUNTS", "")
	test.Setenv("MAGPIE_ADDR", "")
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKMiddleware(test, manager, directory, "1.0.0", descriptor)
	configureSDKMiddleware(test, manager, options)
	return manager
}

func sdkMiddlewareFront(server *Server, body, secret string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Cookie", "PRIVATE-CLIENT-COOKIE")
	request.Header.Set("Proxy-Authorization", "PRIVATE-PROXY-AUTH")
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	return response
}

func readSDKMiddlewareTrace(test *testing.T, filename string) []sdkMiddlewareTrace {
	test.Helper()
	raw, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		test.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE-CLIENT", "PRIVATE-PROXY", "PRIVATE-PROVIDER", "me@example.com", "spare@example.com"} {
		if bytes.Contains(raw, []byte(secret)) {
			test.Fatal("middleware context exposed private credentials or account identity")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var records []sdkMiddlewareTrace
	for {
		var record sdkMiddlewareTrace
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			test.Fatal(err)
		}
		if record.HasAuth || sdk.ValidateMiddlewareScope(record.Scope) != nil {
			test.Fatal("middleware received account or write authority:", record.Scope)
		}
		records = append(records, record)
	}
	return records
}

func TestSDKMiddlewareOrdinaryFrontStreamAndState(test *testing.T) {
	fresh(test)
	directory := buildSDKMiddleware(test)
	trace := filepath.Join(test.TempDir(), "trace")
	manager := setupSDKMiddleware(test, directory, sdkMiddlewareDescriptor(), map[string]any{"trace": trace, "mode": "write"})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if body["temperature"] != 0.25 {
			test.Error("ordinary request did not receive SDK request transform")
		}
		requests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		answer := sse(`data: {"id":"c","model":"m1","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			`data: {"id":"c","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			`data: [DONE]`)
		for index := 0; index < len(answer); index += 7 {
			_, _ = io.WriteString(writer, answer[index:min(index+7, len(answer))])
			writer.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "PRIVATE-PROVIDER",
		Models: []string{"m1"}, Chat: upstream.URL + "/v1"}); err != nil {
		test.Fatal(err)
	}
	server := New()
	for range 2 {
		response := sdkMiddlewareFront(server, `{"model":"fake/m1","stream":true,"messages":[{"role":"user","content":"hello"}]}`, "magpie")
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"sdkState":"request-seen"`) ||
			!strings.Contains(response.Body.String(), "[DONE]") {
			test.Fatalf("ordinary Front did not apply SDK streaming middleware: %d %s", response.Code, response.Body.String())
		}
	}
	records := readSDKMiddlewareTrace(test, trace)
	methods := map[string]int{}
	for _, record := range records {
		methods[record.Method]++
		if !record.WriteDenied {
			test.Fatal("read-only middleware wrote persistent state")
		}
		if record.Request.Context.Account != nil {
			test.Fatal("key request inherited subscription account context")
		}
		if record.Method == sdk.MethodMiddlewareRequest && len(record.Request.State) != 0 {
			test.Fatal("a new request inherited another request's private state")
		}
	}
	if requests.Load() != 2 || methods[sdk.MethodMiddlewareRequest] != 2 || methods[sdk.MethodMiddlewareProvider] != 2 ||
		methods[sdk.MethodMiddlewareEvent] < 4 || methods[sdk.MethodMiddlewareResponse] != 0 {
		test.Fatalf("wrong synchronous lifecycle: %v", methods)
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: "middleware.fixture"}, "middleware.must-not-write")
	if err != nil || state.Version != 0 {
		test.Fatal("middleware persisted a failed write:", err)
	}
	rows := usage.Load(time.Time{})
	if len(rows) != 2 || rows[0].Input != 3 || rows[0].Output != 2 || rows[1].Input != 3 || rows[1].Output != 2 {
		test.Fatalf("middleware changed ordinary accounting: %+v", rows)
	}
}

func TestSDKMiddlewareRejectRollbackAndCallerModelGuard(test *testing.T) {
	directory := buildSDKMiddleware(test)
	for _, mode := range []string{"reject", "protected", "request-error", "oversize-state", "emit"} {
		test.Run(mode, func(test *testing.T) {
			fresh(test)
			trace := filepath.Join(test.TempDir(), "trace")
			descriptor := sdkMiddlewareDescriptor()
			descriptor.Origins = []string{"local", "gateway-key"}
			setupSDKMiddleware(test, directory, descriptor, map[string]any{"mode": mode, "trace": trace})
			var calls, forbidden atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				raw, _ := io.ReadAll(request.Body)
				if strings.Contains(string(raw), `"temperature":0.25`) {
					test.Error("failed request transform was not rolled back")
				}
				calls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"id":"c","model":"m1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer upstream.Close()
			hidden := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				forbidden.Add(1)
				http.Error(writer, "forbidden provider reached", 500)
			}))
			defer hidden.Close()
			for _, current := range []provider.Provider{
				{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m1"}, Chat: upstream.URL + "/v1"},
				{ID: "forbidden", Name: "Forbidden", Key: "private", Models: []string{"m1"}, Chat: hidden.URL + "/v1"},
			} {
				if err := provider.Save(current); err != nil {
					test.Fatal(err)
				}
			}
			keys, secrets := newCaller(test, "Restricted")
			if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"fake/m1"}}); err != nil {
				test.Fatal(err)
			}
			response := sdkMiddlewareFront(New(), `{"model":"fake/m1","messages":[{"role":"user","content":"hello"}]}`, secrets[0])
			if mode == "reject" {
				if response.Code != 403 || calls.Load() != 0 || !strings.Contains(response.Body.String(), "fixture denied request") {
					test.Fatalf("onRequest rejection did not stop ordinary routing: %d %s calls%d", response.Code, response.Body.String(), calls.Load())
				}
			} else if response.Code != 200 || calls.Load() != 1 || forbidden.Load() != 0 ||
				strings.Contains(response.Body.String(), "PRIVATE-MIDDLEWARE") ||
				strings.Contains(response.Body.String(), `"sdkState":"request-seen"`) {
				test.Fatalf("failed middleware changed authorized routing/state: %d %s calls%d forbidden%d",
					response.Code, response.Body.String(), calls.Load(), forbidden.Load())
			}
			_ = readSDKMiddlewareTrace(test, trace)
		})
	}
}

func TestSDKMiddlewareAccountSkipAndProviderReset(test *testing.T) {
	test.Setenv("MAGPIE_NO_ACCOUNTS", "")
	codexSignedIn(test, "spare@example.com")
	directory := buildSDKMiddleware(test)
	trace := filepath.Join(test.TempDir(), "trace")
	setupSDKMiddleware(test, directory, sdkMiddlewareDescriptor(), map[string]any{"trace": trace, "skipFirst": true})
	if err := provider.SetRouting("codex", provider.Ordered); err != nil {
		test.Fatal(err)
	}
	var lock sync.Mutex
	var tried []string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		lock.Lock()
		tried = append(tried, request.Header.Get("chatgpt-account-id"))
		lock.Unlock()
		http.Error(writer, `{"error":{"message":"fixture unavailable"}}`, 500)
	}))
	defer upstream.Close()
	previous := provider.CodexBase
	provider.CodexBase = upstream.URL + "/backend-api/codex"
	test.Cleanup(func() { provider.CodexBase = previous })
	var keyCalls atomic.Int32
	key := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(raw), `"temperature":0.25`) {
			test.Error("per-provider try lost the accepted request")
		}
		keyCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"c","model":"m1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer key.Close()
	if err := provider.Save(provider.Provider{ID: "second", Name: "Second", Key: "k", Models: []string{"m1"}, Chat: key.URL + "/v1"}); err != nil {
		test.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{Name: "SDK reset", Members: []string{"codex/gpt-5.5", "second/m1"}, Routing: provider.Ordered}); err != nil {
		test.Fatal(err)
	}
	response := sdkMiddlewareFront(New(), `{"model":"group/sdk-reset","messages":[{"role":"user","content":"hello"}]}`, "magpie")
	if response.Code != 200 || keyCalls.Load() != 1 || !strings.Contains(response.Body.String(), `"sdkAccount":false`) {
		test.Fatalf("account-to-key fallback did not reset middleware context: %d %s", response.Code, response.Body.String())
	}
	lock.Lock()
	observed := strings.Join(tried, ",")
	lock.Unlock()
	if observed != "acct-2" {
		test.Fatalf("onAccount did not pass the first account over: %q", observed)
	}
	records := readSDKMiddlewareTrace(test, trace)
	accountHooks, keyHooks := 0, 0
	for _, record := range records {
		if record.Method == sdk.MethodMiddlewareAccount {
			accountHooks++
		}
		if record.Request.Context.Provider != nil && record.Request.Context.Provider.ID == "second" {
			keyHooks++
			if record.Request.Context.Account != nil {
				test.Fatal("key provider retained previous account context")
			}
		}
	}
	if accountHooks != 2 || keyHooks < 2 {
		test.Fatalf("ordinary owners omitted account/provider/response hooks: accounts%d key%d", accountHooks, keyHooks)
	}
	if _, found := restOf("codex@me@example.com"); found {
		test.Fatal("middleware-skipped account was rested")
	}
}

func TestSDKMiddlewareProviderTransformsStayWithinTry(test *testing.T) {
	fresh(test)
	directory := buildSDKMiddleware(test)
	trace := filepath.Join(test.TempDir(), "trace")
	setupSDKMiddleware(test, directory, sdkMiddlewareDescriptor(), map[string]any{"trace": trace})
	var first, second atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			test.Error(err)
		}
		switch request.URL.Path {
		case "/first/chat/completions":
			first.Add(1)
			if body["temperature"] != 0.5 {
				test.Error("first provider did not receive its own transform")
			}
			http.Error(writer, `{"error":{"message":"first unavailable"}}`, 500)
		case "/second/chat/completions":
			second.Add(1)
			if body["temperature"] != 0.25 {
				test.Error("second provider inherited another try's transform")
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"c","model":"m1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		default:
			test.Error("unexpected upstream path", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	for _, id := range []string{"first", "second"} {
		if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "PRIVATE-PROVIDER",
			Models: []string{"m1"}, Chat: upstream.URL + "/" + id}); err != nil {
			test.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{Name: "SDK provider isolation",
		Members: []string{"first/m1", "second/m1"}, Routing: provider.Ordered}); err != nil {
		test.Fatal(err)
	}
	response := sdkMiddlewareFront(New(), `{"model":"group/sdk-provider-isolation","messages":[{"role":"user","content":"hello"}]}`, "magpie")
	if response.Code != 200 || first.Load() != 1 || second.Load() != 1 {
		test.Fatalf("ordinary fallback did not try both providers: %d %s first%d second%d",
			response.Code, response.Body.String(), first.Load(), second.Load())
	}
	var providers []string
	for _, record := range readSDKMiddlewareTrace(test, trace) {
		if record.Method == sdk.MethodMiddlewareProvider && record.Request.Context.Provider != nil {
			providers = append(providers, record.Request.Context.Provider.ID)
		}
	}
	if strings.Join(providers, ",") != "first,second" {
		test.Fatal("middleware provider context did not follow ordinary fallback:", providers)
	}
}
