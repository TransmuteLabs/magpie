package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sdkHookConfig struct {
	Marker      string `json:"marker"`
	RequestURL  string `json:"requestURL"`
	ResponseURL string `json:"responseURL"`
	SignInURL   string `json:"signInURL"`
	Protected   bool   `json:"protected"`
}

type sdkHookDelivery struct {
	Actor         string               `json:"actor"`
	Request       *sdk.GatewayRequest  `json:"request,omitempty"`
	Response      *sdk.GatewayResponse `json:"response,omitempty"`
	SignIn        *sdk.GatewaySignIn   `json:"signIn,omitempty"`
	Scope         sdk.Scope            `json:"scope"`
	Auth          json.RawMessage      `json:"auth"`
	LegacyRequest json.RawMessage      `json:"legacyRequest,omitempty"`
	LegacyReply   json.RawMessage      `json:"legacyReply,omitempty"`
}

func installSDKGatewayHook(t *testing.T, config sdkHookConfig, descriptor sdk.GatewayHookDescriptor) *extension.Manager {
	t.Helper()
	// A successful SDK-only request with this path cannot have loaded Bun.
	t.Setenv("MAGPIE_BUN", filepath.Join(t.TempDir(), "no-bun"))
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the SDK hook fixture requires an existing Go toolchain")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate SDK hook fixture source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, tool, "build", "-o", filepath.Join(source, entry), "./internal/gateway/testdata/extension-hooks")
	command.Dir = root
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build SDK hook fixture: %v\n%s", err, output)
	}
	if descriptor.TimeoutMs == 0 {
		descriptor.TimeoutMs = 10000
	}
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "sdk-gateway-hook", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 2}, Capabilities: []sdk.Capability{sdk.GatewayHooks}},
		Hooks:    &descriptor,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
	if _, err := manager.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	values, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(ctx, manifest.ID, "0", values); err != nil {
		t.Fatal(err)
	}
	return manager
}

func closeSDKGatewayHook(t *testing.T, manager *extension.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Error(err)
	}
}

func receiveSDKHook(t *testing.T, deliveries <-chan sdkHookDelivery) sdkHookDelivery {
	t.Helper()
	select {
	case delivery := <-deliveries:
		if delivery.Actor == "sdk" && (delivery.Scope.Provider != "" || delivery.Scope.Account != "" ||
			delivery.Scope.Generation != 0 || len(delivery.Auth) != 0 && string(delivery.Auth) != "null") {
			t.Fatalf("gateway hook received credential authority: %+v", delivery)
		}
		return delivery
	case <-time.After(20 * time.Second):
		t.Fatal("SDK hook did not reach its local fixture")
		return sdkHookDelivery{}
	}
}

func waitSDKHookCompleted(t *testing.T, manager *extension.Manager, scope sdk.Scope) {
	t.Helper()
	if scope.Operation == "" {
		t.Fatal("SDK observer has no host-owned operation")
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		entry, err := manager.Store().Operation(scope.Plugin, scope.Operation)
		if err != nil {
			t.Fatal(err)
		}
		switch entry.State {
		case "completed":
			return
		case "uncertain", "not-started":
			t.Fatalf("observer did not survive client EOF: %s", entry.State)
		}
	}
	t.Fatal("observer did not complete after its blocking server was released")
}

func sdkHookRequest(ctx context.Context, base string, stream bool) *http.Request {
	body := fmt.Sprintf(`{"model":"fake/m1","stream":%v,"origin":"local","messages":[{"role":"user","content":"SDK question"}]}`, stream)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer magpie")
	request.Header.Set("Cookie", "private-cookie-value")
	request.Header.Set("Proxy-Authorization", "Basic private-proxy-value")
	request.Header.Set(SessionHeader, "sdk-hook-session")
	return request
}

func sdkHookProvider(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "private-provider-value", Models: []string{"m1"}, Chat: server.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	return server
}

func TestSDKGatewayHooksObserveStreaming(t *testing.T) {
	fresh(t)
	deliveries := make(chan sdkHookDelivery, 4)
	observeRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseObserver := func() { releaseOnce.Do(func() { close(observeRelease) }) }
	defer releaseObserver()
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value sdkHookDelivery
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			t.Error(err)
			return
		}
		deliveries <- value
		select {
		case <-observeRelease:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(observer.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{Marker: "stream", ResponseURL: observer.URL}, sdk.GatewayHookDescriptor{Request: true, Response: true})
	generated := make(chan struct{})
	var generatedOnce sync.Once
	finishGeneration := func() { generatedOnce.Do(func() { close(generated) }) }
	defer finishGeneration()
	upstreamBody := make(chan []byte, 1)
	sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(`data: {"id":"s1","model":"m1","choices":[{"index":0,"delta":{"content":"live SDK answer"}}]}`))
		w.(http.Flusher).Flush()
		select {
		case <-generated:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, sse(`data: {"id":"s1","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "data: [DONE]"))
	})
	gateway := httptest.NewServer(lanGuard(New().Handler()))
	defer func() {
		finishGeneration()
		releaseObserver()
		gateway.CloseClientConnections()
		gateway.Close()
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, ended := make(chan string, 1), make(chan error, 1)
	go func() {
		response, err := gateway.Client().Do(sdkHookRequest(ctx, gateway.URL, true))
		if err != nil {
			first <- err.Error()
			ended <- err
			return
		}
		buf := make([]byte, 4096)
		n, err := response.Body.Read(buf)
		first <- string(buf[:n])
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
		}
		response.Body.Close()
		ended <- err
	}()
	select {
	case delta := <-first:
		if !strings.Contains(delta, "live SDK answer") {
			t.Fatalf("first delta: %s", delta)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SDK recall prevented the first live delta")
	}
	if body := <-upstreamBody; !bytes.Contains(body, []byte("sdk recall: stream")) {
		t.Fatalf("SDK recall did not reach inference: %s", body)
	}
	finishGeneration()
	delivery := receiveSDKHook(t, deliveries)
	if delivery.Response == nil || !delivery.Response.Success || delivery.Response.Context.Origin != "local" ||
		delivery.Response.Context.Query != "SDK question" || !bytes.Contains(delivery.Response.Request, []byte("sdk recall: stream")) ||
		!bytes.Contains(delivery.Response.Reply, []byte("live SDK answer")) {
		t.Fatalf("completed SDK exchange: %+v", delivery)
	}
	encoded, _ := json.Marshal(delivery)
	for _, secret := range []string{"private-cookie-value", "private-proxy-value", "private-provider-value", "Bearer magpie"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("hook payload contains HTTP authority: %s", secret)
		}
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked SDK observer delayed client EOF")
	}
	// The original HTTP context ends, while this same observer is still held.
	cancel()
	releaseObserver()
	waitSDKHookCompleted(t, manager, delivery.Scope)
	closeSDKGatewayHook(t, manager)
}

func TestSDKGatewayRequestCancellation(t *testing.T) {
	fresh(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblockRecall := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblockRecall()
	var recalls atomic.Int32
	prepare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if recalls.Add(1) == 1 {
			close(started)
			select {
			case <-r.Context().Done():
				close(stopped)
			case <-release:
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(prepare.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{Marker: "after-cancel", RequestURL: prepare.URL}, sdk.GatewayHookDescriptor{Request: true, Response: true})
	var inference atomic.Int32
	sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
		inference.Add(1)
		_, _ = io.WriteString(w, `{"id":"ok","choices":[{"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}]}`)
	})
	gateway := httptest.NewServer(lanGuard(New().Handler()))
	defer func() {
		unblockRecall()
		gateway.CloseClientConnections()
		gateway.Close()
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		response, err := gateway.Client().Do(sdkHookRequest(ctx, gateway.URL, false))
		if response != nil {
			response.Body.Close()
		}
		ended <- err
	}()
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("SDK request did not enter blocking recall")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("client cancellation did not reach SDK recall I/O")
	}
	if err := <-ended; err == nil {
		t.Fatal("canceled client request succeeded")
	}
	if inference.Load() != 0 {
		t.Fatal("inference started for an abandoned request hook")
	}
	response, err := gateway.Client().Do(sdkHookRequest(t.Context(), gateway.URL, false))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(body, []byte("healthy")) || inference.Load() != 1 {
		t.Fatalf("independent request after cancellation: %d %s", response.StatusCode, body)
	}
	closeSDKGatewayHook(t, manager)
}

func TestSDKGatewayLegacyChainOrderingAndFailure(t *testing.T) {
	bun := testenv.Bun(t) // installed only; no package/runtime installation
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-failure=%v", failed), func(t *testing.T) {
			fresh(t)
			deliveries := make(chan sdkHookDelivery, 4)
			observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value sdkHookDelivery
				if json.NewDecoder(r.Body).Decode(&value) != nil {
					t.Error("invalid hook delivery")
					return
				}
				deliveries <- value
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(observer.Close)
			manager := installSDKGatewayHook(t, sdkHookConfig{Marker: "chain", ResponseURL: observer.URL}, sdk.GatewayHookDescriptor{Request: true, Response: true})
			t.Setenv("MAGPIE_BUN", bun)
			t.Cleanup(plugin.Settle)
			file := filepath.Join(t.TempDir(), "legacy.hooks.js")
			script := fmt.Sprintf(`export function onRequest(body) {
 body.messages.push({role:"system",content:"legacy recall"});
 if (%v) throw new Error("private failure text");
 return body;
}
export async function onResponse(reply,ctx) {
 await fetch(%q,{method:"POST",body:JSON.stringify({actor:"legacy",legacyRequest:ctx.request,legacyReply:reply}),signal:ctx.signal});
}`, failed, observer.URL)
			if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := plugin.Add(t.Context(), file); err != nil {
				t.Fatal(err)
			}
			input := make(chan []byte, 1)
			sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				input <- body
				_, _ = io.WriteString(w, `{"id":"ok","choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`)
			})
			gateway := httptest.NewServer(lanGuard(New().Handler()))
			defer func() { gateway.CloseClientConnections(); gateway.Close() }()
			response, err := gateway.Client().Do(sdkHookRequest(t.Context(), gateway.URL, false))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("HTTP %d", response.StatusCode)
			}
			body := <-input
			if !bytes.Contains(body, []byte("sdk recall: chain")) || bytes.Contains(body, []byte("legacy recall")) == failed {
				t.Fatalf("request chain did not retain only accepted changes: %s", body)
			}
			want := 2
			if failed {
				want = 1
			}
			for range want {
				got := receiveSDKHook(t, deliveries)
				if got.Actor == "legacy" {
					if failed || bytes.Contains(got.LegacyRequest, []byte("sdk recall")) || !bytes.Contains(got.LegacyRequest, []byte("legacy recall")) {
						t.Fatalf("legacy observer received the SDK stage's request: %s", got.LegacyRequest)
					}
				} else if got.Response == nil || !got.Response.Success || !bytes.Contains(got.Response.Request, []byte("sdk recall: chain")) {
					t.Fatalf("SDK observer lost its accepted request: %+v", got)
				}
			}
			closeSDKGatewayHook(t, manager)
			plugin.Settle()
			if len(deliveries) != 0 {
				t.Fatal("a failed or completed hook was notified twice")
			}
		})
	}
}

func TestSDKGatewayRemoteOriginSkipsMemory(t *testing.T) {
	fresh(t)
	requests := make(chan sdkHookDelivery, 1)
	recall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value sdkHookDelivery
		_ = json.NewDecoder(r.Body).Decode(&value)
		requests <- value
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recall.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{Marker: "private memory", RequestURL: recall.URL}, sdk.GatewayHookDescriptor{Request: true})
	_, secrets := newCaller(t, "SDK remote caller")
	input := make(chan []byte, 1)
	sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		input <- body
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"public answer"},"finish_reason":"stop"}]}`)
	})
	gateway := httptest.NewServer(lanGuard(New().Handler()))
	defer func() { gateway.CloseClientConnections(); gateway.Close() }()
	request := sdkHookRequest(t.Context(), gateway.URL, false)
	request.Header.Set("Authorization", "Bearer "+secrets[0])
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("HTTP %d", response.StatusCode)
	}
	if body := <-input; bytes.Contains(body, []byte("sdk recall")) {
		t.Fatalf("client-supplied local origin gained memory access: %s", body)
	}
	if manager.Running("sdk-gateway-hook") {
		t.Fatal("ineligible origin started an SDK worker")
	}
	closeSDKGatewayHook(t, manager)
	if len(requests) != 0 {
		t.Fatal("remote request was disclosed to local memory")
	}
}

func TestSDKGatewayBrowserMemoryPermission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		browser    bool
		origins    []string
		wantRecall bool
	}{
		{"browser default denied", true, nil, false},
		{"browser explicitly allowed", true, []string{"browser"}, true},
		{"Node fetch mode alone", false, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			requests := make(chan sdkHookDelivery, 1)
			recall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value sdkHookDelivery
				_ = json.NewDecoder(r.Body).Decode(&value)
				requests <- value
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(recall.Close)
			manager := installSDKGatewayHook(t, sdkHookConfig{Marker: "browser policy", RequestURL: recall.URL},
				sdk.GatewayHookDescriptor{Request: true, Origins: tc.origins})
			input := make(chan []byte, 1)
			sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				input <- body
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`)
			})
			gateway := httptest.NewServer(lanGuard(New().Handler()))
			defer func() { gateway.CloseClientConnections(); gateway.Close() }()
			request := sdkHookRequest(t.Context(), gateway.URL, false)
			request.Header.Set("Sec-Fetch-Mode", "cors")
			if tc.browser {
				request.Header.Set("Origin", gateway.URL)
				request.Header.Set("Sec-Fetch-Site", "same-origin")
				request.Header.Set("User-Agent", "Mozilla/5.0 fixture")
			}
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("HTTP %d", response.StatusCode)
			}
			if body := <-input; bytes.Contains(body, []byte("sdk recall: browser policy")) != tc.wantRecall {
				t.Fatalf("SDK browser permission changed the wrong request: %s", body)
			}
			if tc.wantRecall {
				got := receiveSDKHook(t, requests)
				wantOrigin := "local"
				if tc.browser {
					wantOrigin = "browser"
				}
				if got.Request == nil || got.Request.Context.Origin != wantOrigin {
					t.Fatalf("SDK origin: %+v; want %s", got.Request, wantOrigin)
				}
			} else if manager.Running("sdk-gateway-hook") {
				t.Fatal("default-denied browser started a memory worker")
			}
			closeSDKGatewayHook(t, manager)
			if len(requests) != 0 {
				t.Fatal("memory received an undeclared or duplicate browser request")
			}
		})
	}
}

func TestSDKGatewayProtectedChangeRollsBack(t *testing.T) {
	fresh(t)
	deliveries := make(chan sdkHookDelivery, 1)
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value sdkHookDelivery
		_ = json.NewDecoder(r.Body).Decode(&value)
		deliveries <- value
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(observer.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{Marker: "must roll back", Protected: true, ResponseURL: observer.URL}, sdk.GatewayHookDescriptor{Request: true, Response: true})
	input := make(chan []byte, 1)
	sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		input <- body
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"unchanged route"},"finish_reason":"stop"}]}`)
	})
	gateway := httptest.NewServer(lanGuard(New().Handler()))
	defer func() { gateway.CloseClientConnections(); gateway.Close() }()
	response, err := gateway.Client().Do(sdkHookRequest(t.Context(), gateway.URL, false))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("protected hook changed routing: HTTP %d", response.StatusCode)
	}
	if body := <-input; bytes.Contains(body, []byte("sdk recall")) || bytes.Contains(body, []byte("unauthorized-model")) {
		t.Fatalf("failed SDK hook did not roll back atomically: %s", body)
	}
	closeSDKGatewayHook(t, manager)
	if len(deliveries) != 0 {
		t.Fatal("a rolled-back SDK hook received a completion notification")
	}
}

func TestSDKGatewayResponseOnly(t *testing.T) {
	fresh(t)
	deliveries := make(chan sdkHookDelivery, 1)
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value sdkHookDelivery
		_ = json.NewDecoder(r.Body).Decode(&value)
		deliveries <- value
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(observer.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{ResponseURL: observer.URL}, sdk.GatewayHookDescriptor{Response: true})
	sdkHookProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"capture only"},"finish_reason":"stop"}]}`)
	})
	gateway := httptest.NewServer(lanGuard(New().Handler()))
	defer func() { gateway.CloseClientConnections(); gateway.Close() }()
	response, err := gateway.Client().Do(sdkHookRequest(t.Context(), gateway.URL, false))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("HTTP %d", response.StatusCode)
	}
	delivery := receiveSDKHook(t, deliveries)
	if delivery.Response == nil || !delivery.Response.Success || bytes.Contains(delivery.Response.Request, []byte("sdk recall")) ||
		delivery.Response.Context.Query != "SDK question" {
		t.Fatalf("response-only SDK hook lost the original exchange: %+v", delivery)
	}
	closeSDKGatewayHook(t, manager)
}

func TestSDKGatewaySignInLoopWithoutLegacyHost(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	deliveries := make(chan sdkHookDelivery, 4)
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value sdkHookDelivery
		if json.NewDecoder(r.Body).Decode(&value) != nil {
			t.Error("invalid sign-in delivery")
			return
		}
		deliveries <- value
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(observer.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{SignInURL: observer.URL}, sdk.GatewayHookDescriptor{SignIn: true})
	access := codexAccessToken(t)
	revokedUpstream(t, access)
	if response := codexPostTo(New(), `{"model":"gpt-5.5","stream":true,"input":"ping"}`); response.Code != 200 {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body)
	}
	tellHooksOfSignIns(t.Context())
	tellHooksOfSignIns(t.Context())
	event := receiveSDKHook(t, deliveries)
	if event.SignIn == nil || event.SignIn.Event.Provider != "codex" || event.SignIn.Event.Account != "me@example.com" || event.SignIn.Event.State != "signed-out" {
		t.Fatalf("sign-in loop did not reach SDK-only notification: %+v", event)
	}
	encoded, _ := json.Marshal(event)
	for _, private := range []string{access, "r-acct", "token_revoked", "invalidated"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatalf("SDK sign-in event disclosed %q", private)
		}
	}
	expires := time.Date(2026, 11, 20, 12, 30, 0, 0, time.FixedZone("fixture", 3*60*60))
	gatewaySignIn(t.Context(), plugin.SignInEvent{Provider: "claude", ProviderName: "Claude", Account: "owner@example.com", State: "ending", EndsAt: &expires})
	ending := receiveSDKHook(t, deliveries)
	if ending.SignIn == nil || ending.SignIn.Event.EndsAt != expires.Format(time.RFC3339) {
		t.Fatalf("legacy sign-in expiry did not retain RFC3339 semantics: %+v", ending.SignIn)
	}
	closeSDKGatewayHook(t, manager)
	if len(deliveries) != 0 {
		t.Fatal("sign-in loop repeated an already-notified account")
	}
}
