package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/protocolext"
)

type fixtureInbound struct {
	id, path string
	contract sdk.Contract
	decode   func(context.Context, http.Header, []byte) (sdk.Request, error)
	finish   func(error) error
}

func (adapter fixtureInbound) Contract() sdk.Contract {
	if adapter.contract.Capabilities == nil {
		return sdk.InboundContract()
	}
	return adapter.contract
}

func (adapter fixtureInbound) ID() string {
	if adapter.id != "" {
		return adapter.id
	}
	return "fixture"
}

func (adapter fixtureInbound) Path() string {
	if adapter.path != "" {
		return adapter.path
	}
	return "/fixture/rpc"
}

func (adapter fixtureInbound) Decode(ctx context.Context, headers http.Header, body []byte) (sdk.Request, error) {
	if adapter.decode != nil {
		return adapter.decode(ctx, headers, body)
	}
	canonical, err := json.Marshal(map[string]any{
		"model": string(body), "messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	return sdk.Request{Protocol: sdk.Chat, Body: canonical, Session: "native-session"}, err
}

func (adapter fixtureInbound) Reply(writer http.ResponseWriter) sdk.Response {
	return fixtureReply{ResponseWriter: writer, finish: adapter.finish}
}

type fixtureReply struct {
	http.ResponseWriter
	finish func(error) error
}

func (reply fixtureReply) Finish(cause error) error {
	if reply.finish != nil {
		return reply.finish(cause)
	}
	return nil
}

func (reply fixtureReply) Flush() {
	http.NewResponseController(reply.ResponseWriter).Flush()
}

func protocolServer(t *testing.T, adapters ...sdk.Inbound) *Server {
	t.Helper()
	registry, err := sdk.New(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewWithProtocols(registry)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func protocolPost(handler http.Handler, path, body, secret string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/x-fixture")
	request.Header.Set("User-Agent", "fixture-client")
	request.Header.Set("X-Native-Secret", "native-credential")
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestProtocolBridgeThroughFront(t *testing.T) {
	fresh(t)
	type observation struct {
		path, body string
		headers    http.Header
	}
	observed := make(chan observation, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		observed <- observation{request.URL.RequestURI(), string(body), request.Header.Clone()}
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)
	if err := provider.Save(provider.Provider{
		ID: "fixture", Name: "Fixture", Key: "provider-key", Chat: upstream.URL + "/v1", Models: []string{"m1"},
	}); err != nil {
		t.Fatal(err)
	}
	finished := 0
	adapter := fixtureInbound{finish: func(cause error) error {
		finished++
		if cause != nil {
			t.Fatal(cause)
		}
		return nil
	}}
	response := protocolPost(protocolServer(t, adapter).Front(), adapter.Path()+"?native=secret", "fixture/m1", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ok") {
		t.Fatalf("normalized response: %d %s", response.Code, response.Body)
	}
	select {
	case got := <-observed:
		if got.path != "/v1/chat/completions" || modelOf([]byte(got.body)) != "m1" {
			t.Fatalf("provider request: %+v", got)
		}
		if strings.Contains(got.body, "native-credential") || got.headers.Get("X-Native-Secret") != "" || got.headers.Get("Authorization") != "Bearer provider-key" {
			t.Fatalf("native credentials escaped: %+v", got)
		}
	default:
		t.Fatal("canonical provider was not reached")
	}
	if finished != 1 {
		t.Fatalf("Finish called %d times", finished)
	}
}

func TestProtocolBridgeKeepsCallerRestrictions(t *testing.T) {
	fresh(t)
	calls := limitedUpstream(t, 0)
	keys, secrets := newCaller(t, "Read only", "Model held", "Capped")
	if _, err := access.Update("access-key", access.Change{Key: keys[0].ID, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("models-key", access.Change{Key: keys[1].ID, Models: []string{"elsewhere/*"}}); err != nil {
		t.Fatal(err)
	}
	budget.Forget()
	pinLimitClock(t)
	setLimit(t, keys[2].ID, &access.Limit{Period: "day", Tokens: 1, Models: []string{"plan/m1"}})
	decodes := 0
	adapter := fixtureInbound{decode: func(context.Context, http.Header, []byte) (sdk.Request, error) {
		decodes++
		return sdk.Request{Protocol: sdk.Chat, Body: []byte(`{"model":"plan/m1","messages":[{"role":"user","content":"hello"}]}`)}, nil
	}}
	handler := protocolServer(t, adapter).Front()
	if response := protocolPost(handler, adapter.Path(), "native", secrets[2]); response.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("initial budget: %d calls=%d %s", response.Code, calls.Load(), response.Body)
	}
	for index, want := range []int{http.StatusForbidden, http.StatusForbidden, http.StatusTooManyRequests} {
		response := protocolPost(handler, adapter.Path(), "native", secrets[index])
		if response.Code != want {
			t.Errorf("key %d: got %d, want %d: %s", index, response.Code, want, response.Body)
		}
	}
	browserSecret, err := access.Update("add-key", access.Change{Name: "Browser only", Browser: true})
	if err != nil {
		t.Fatal(err)
	}
	if response := protocolPost(handler, adapter.Path(), "native", browserSecret); response.Code != http.StatusForbidden {
		t.Fatalf("browser key acquired protocol access: %d %s", response.Code, response.Body)
	}
	beforeGuards := decodes
	for _, test := range []struct {
		peer, origin string
		pagePort     bool
	}{
		{peer: "192.0.2.20:1234"},
		{peer: "127.0.0.1:1234", origin: "https://untrusted.example"},
		{peer: "127.0.0.1:1234", origin: "null"},
		{peer: "127.0.0.1:1234", pagePort: true},
	} {
		request := httptest.NewRequest(http.MethodPost, adapter.Path(), strings.NewReader("native"))
		request.RemoteAddr = test.peer
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		if test.pagePort {
			request = FromPagePort(request)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || response.Code >= 500 {
			t.Errorf("guard %+v: got %d %s", test, response.Code, response.Body)
		}
	}
	if decodes != beforeGuards || decodes != 3 || calls.Load() != 1 {
		t.Fatalf("guards reached codec/provider: decodes=%d calls=%d", decodes, calls.Load())
	}
}

func TestProtocolBridgeCatalogAliasesBeforeScopedBudget(t *testing.T) {
	fresh(t)
	calls := limitedUpstream(t, 0)
	plan, err := provider.Find("plan")
	if err != nil {
		t.Fatal(err)
	}
	plan.Models = []string{"m1", "m2"}
	if err := provider.Save(*plan); err != nil {
		t.Fatal(err)
	}
	keys, secrets := newCaller(t, "Aliased client")
	if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"plan/m1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("aliases-key", access.Change{Key: keys[0].ID, Aliases: map[string]string{"assistant": "plan/m1"}}); err != nil {
		t.Fatal(err)
	}
	budget.Forget()
	pinLimitClock(t)
	setLimit(t, keys[0].ID, &access.Limit{Period: "day", Tokens: 1, Models: []string{"plan/m1"}})
	chat := fixtureInbound{}
	catalog := fixtureInbound{id: "catalog", path: "/fixture/models", contract: sdk.CatalogContract(),
		decode: func(context.Context, http.Header, []byte) (sdk.Request, error) {
			return sdk.Request{Protocol: sdk.Models}, nil
		}}
	handler := protocolServer(t, chat, catalog).Front()
	response := protocolPost(handler, catalog.Path(), "", secrets[0])
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"assistant"`) || strings.Contains(response.Body.String(), `"id":"plan/m1"`) || strings.Contains(response.Body.String(), `"id":"plan/m2"`) || calls.Load() != 0 {
		t.Fatalf("authorized catalog: %d calls=%d %s", response.Code, calls.Load(), response.Body)
	}
	first := protocolPost(handler, chat.Path(), "assistant", secrets[0])
	if first.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("discovered alias: %d calls=%d %s", first.Code, calls.Load(), first.Body)
	}
	second := protocolPost(handler, chat.Path(), "assistant", secrets[0])
	if second.Code != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("alias bypassed model budget: %d calls=%d %s", second.Code, calls.Load(), second.Body)
	}
}

func TestProtocolBridgeKeepsNativeJSONBeforeCanonicalAliases(t *testing.T) {
	for _, entry := range []struct {
		name, path, peer string
		handler          func(*Server) http.Handler
	}{
		{name: "front", path: "/fixture/rpc", peer: "192.0.2.20:1234", handler: (*Server).Front},
		{name: "handler", path: "/fixture/rpc", peer: "127.0.0.1:1234", handler: (*Server).Handler},
	} {
		t.Run(entry.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "0.0.0.0:3499")
			calls := limitedUpstream(t, 0)
			keys, secrets := newCaller(t, "Native JSON", "Standard JSON")
			for _, key := range keys {
				if _, err := access.Update("models-key", access.Change{Key: key.ID, Models: []string{"plan/m1"}}); err != nil {
					t.Fatal(err)
				}
				if _, err := access.Update("aliases-key", access.Change{Key: key.ID, Aliases: map[string]string{
					"native-model": "plan/m1", "assistant": "plan/m1",
				}}); err != nil {
					t.Fatal(err)
				}
			}
			budget.Forget()
			pinLimitClock(t)
			setLimit(t, keys[0].ID, &access.Limit{Period: "day", Tokens: 1, Models: []string{"plan/m1"}})
			const native = `{ "model": "native-model", "prompt": "hello", "metadata": {"format":"native"} }`
			var decodedBodies []string
			adapter := fixtureInbound{decode: func(ctx context.Context, _ http.Header, body []byte) (sdk.Request, error) {
				decodedBodies = append(decodedBodies, string(body))
				if who := access.Caller(ctx); who.KeyID != keys[0].ID {
					t.Errorf("codec lost authenticated caller: %+v", who)
				}
				if string(body) != native {
					return sdk.Request{}, &sdk.Error{Status: http.StatusUnprocessableEntity, Message: "native JSON changed before Decode"}
				}
				return sdk.Request{Protocol: sdk.Chat, Body: []byte(`{"model":"assistant","messages":[{"role":"user","content":"hello"}]}`)}, nil
			}}
			handler := entry.handler(protocolServer(t, adapter))
			post := func(path, body, secret string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.RemoteAddr = entry.peer
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+secret)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			first := post(entry.path, native, secrets[0])
			if first.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("native JSON/canonical alias: status=%d calls=%d decoded=%q response=%s", first.Code, calls.Load(), decodedBodies, first.Body)
			}
			second := post(entry.path, native, secrets[0])
			if second.Code != http.StatusTooManyRequests || calls.Load() != 1 || len(decodedBodies) != 2 || decodedBodies[1] != native {
				t.Fatalf("canonical alias bypassed scoped budget: status=%d calls=%d decoded=%q response=%s", second.Code, calls.Load(), decodedBodies, second.Body)
			}
			standard := post("/v1/chat/completions", `{"model":"assistant","messages":[{"role":"user","content":"hello"}]}`, secrets[1])
			if standard.Code != http.StatusOK || calls.Load() != 2 || len(decodedBodies) != 2 {
				t.Fatalf("standard alias changed: status=%d calls=%d decodes=%d response=%s", standard.Code, calls.Load(), len(decodedBodies), standard.Body)
			}
		})
	}
}

func TestProtocolBridgePreservesContextAndCanonicalEnvelope(t *testing.T) {
	fresh(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = access.WithIdentity(ctx, access.Identity{KeyID: "original-key"})
	request := httptest.NewRequest(http.MethodPost, "/fixture/rpc?control=admin", strings.NewReader("native")).WithContext(ctx)
	request.RemoteAddr = "192.0.2.10:12345"
	request.Header.Set("X-Native-Secret", "do-not-forward")
	request.Trailer = http.Header{"X-Native-Trailer": {"do-not-forward"}}
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("native")), nil }
	finished := 0
	adapter := fixtureInbound{decode: func(got context.Context, headers http.Header, body []byte) (sdk.Request, error) {
		if got != ctx || string(body) != "native" || headers.Get("X-Native-Secret") != "do-not-forward" {
			t.Fatal("codec lost the original bounded request/context")
		}
		headers.Set("X-Native-Secret", "mutated")
		return sdk.Request{Protocol: sdk.Chat, Body: []byte(`{"model":"p/m","messages":[]}`), Session: "native-session"}, nil
	}, finish: func(cause error) error {
		finished++
		if !errors.Is(cause, context.Canceled) {
			t.Fatalf("Finish cause: %v", cause)
		}
		return nil
	}}
	reached := false
	next := http.HandlerFunc(func(writer http.ResponseWriter, got *http.Request) {
		reached = true
		if access.Caller(got.Context()).KeyID != "original-key" || got.RemoteAddr != request.RemoteAddr || got.Host != request.Host {
			t.Fatalf("bridge changed authenticated identity/peer: %+v", got)
		}
		if got.Method != http.MethodPost || got.URL.Path != "/v1/chat/completions" || got.URL.RawQuery != "" || got.RequestURI != "/v1/chat/completions" {
			t.Fatalf("bridge did not normalize the target: %+v", got)
		}
		if got.GetBody != nil || got.Trailer != nil || got.Header.Get("X-Native-Secret") != "" || got.Header.Get(SessionHeader) != "native-session" {
			t.Fatalf("bridge leaked native envelope/session: %+v", got)
		}
		cancel()
		if got.Context().Err() != context.Canceled {
			t.Fatal("cancellation did not reach canonical request")
		}
		io.WriteString(writer, "ok")
	})
	protocolServer(t, adapter).protocolEntry(next).ServeHTTP(httptest.NewRecorder(), request)
	if !reached || finished != 1 || request.Header.Get("X-Native-Secret") != "do-not-forward" {
		t.Fatalf("dispatched=%v Finish=%d original headers=%v", reached, finished, request.Header)
	}
}

func TestProtocolBridgeKeepsForwardedProvenanceAndRemoteSession(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_TRUST_PROXY", "")
	ctx := access.WithIdentity(context.Background(), access.Identity{KeyID: "sharing-key", Default: true})
	ctx = context.WithValue(ctx, callerCtx{}, caller{agent: "fixture-client", hops: 2})
	adapter := fixtureInbound{}
	reached := 0
	next := http.HandlerFunc(func(_ http.ResponseWriter, got *http.Request) {
		reached++
		if local(got) || ownComputer(got) || gatewayOrigin(got) != "gateway-key" {
			t.Fatalf("forwarded client gained owner provenance: local=%v own=%v origin=%q", local(got), ownComputer(got), gatewayOrigin(got))
		}
		remote := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		passOnCaller(got.Context(), remote)
		if remote.Header.Get(SessionHeader) != "native-session" || remote.Header.Get(HopsHeader) != "3" || callerOf(got).agent != "fixture-client" {
			t.Fatalf("remote forwarding changed session/caller: %v caller=%+v", remote.Header, callerOf(got))
		}
	})
	handler := protocolServer(t, adapter).protocolEntry(next)
	for _, header := range append(append([]string(nil), forwardHeaders...), PagePortHeader) {
		request := httptest.NewRequest(http.MethodPost, adapter.Path(), strings.NewReader("p/m")).WithContext(ctx)
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set(header, "192.0.2.20")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if reached != len(forwardHeaders)+1 {
		t.Fatalf("dispatched %d requests", reached)
	}
}

func TestProtocolBridgeKeepsExistingSessionPrecedence(t *testing.T) {
	fresh(t)
	adapter := fixtureInbound{}
	for _, header := range append([]string{SessionHeader}, sessionHeaders...) {
		request := httptest.NewRequest(http.MethodPost, adapter.Path(), strings.NewReader("p/m"))
		request.Header.Set(header, "original-session")
		reached := false
		next := http.HandlerFunc(func(_ http.ResponseWriter, normalized *http.Request) {
			reached = true
			remote := httptest.NewRequest(http.MethodPost, "/", nil)
			passOnCaller(normalized.Context(), remote)
			if sessionOf(normalized.Header) != "original-session" || callerOf(normalized).session != "original-session" || remote.Header.Get(SessionHeader) != "original-session" {
				t.Fatalf("%s: lost existing session %v caller=%+v", header, normalized.Header, callerOf(normalized))
			}
			if header != SessionHeader && normalized.Header.Get(header) != "original-session" {
				t.Fatalf("lost native session header %s", header)
			}
		})
		withCaller(protocolServer(t, adapter).protocolEntry(next)).ServeHTTP(httptest.NewRecorder(), request)
		if !reached {
			t.Fatal("session request was not dispatched")
		}
	}
}

func TestProtocolBridgeRefusesInvalidOperations(t *testing.T) {
	fresh(t)
	for _, test := range []struct {
		name     string
		contract sdk.Contract
		decoded  sdk.Request
	}{
		{name: "unknown", decoded: sdk.Request{Protocol: "unknown"}},
		{name: "invalid JSON", decoded: sdk.Request{Protocol: sdk.Chat, Body: []byte(`{`)}},
		{name: "oversized JSON", decoded: sdk.Request{Protocol: sdk.Chat, Body: []byte(`"` + strings.Repeat("x", protocolBodyLimit) + `"`)}},
		{name: "catalog without capability", decoded: sdk.Request{Protocol: sdk.Models}},
		{name: "catalog with body", contract: sdk.CatalogContract(), decoded: sdk.Request{Protocol: sdk.Models, Body: []byte(`{}`)}},
		{name: "negative timeout", decoded: sdk.Request{Protocol: sdk.Chat, Body: []byte(`{}`), Timeout: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			finished, reached := 0, false
			adapter := fixtureInbound{contract: test.contract,
				decode: func(context.Context, http.Header, []byte) (sdk.Request, error) { return test.decoded, nil },
				finish: func(error) error { finished++; return nil }}
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
			response := protocolPost(protocolServer(t, adapter).protocolEntry(next), adapter.Path(), "", "")
			if reached || response.Code != http.StatusBadGateway || finished != 1 {
				t.Fatalf("reached=%v status=%d Finish=%d: %s", reached, response.Code, finished, response.Body)
			}
		})
	}
}

func TestProtocolBridgeBoundsInputAndHidesPrivateErrors(t *testing.T) {
	fresh(t)
	for _, test := range []struct {
		name, method, path, body string
		limit                    int64
		decodeError              error
		want, decodes            int
	}{
		{name: "bounded", method: "POST", path: "/fixture/rpc", body: "too large", limit: 4, want: http.StatusRequestEntityTooLarge},
		{name: "method", method: "GET", path: "/fixture/rpc", want: http.StatusMethodNotAllowed},
		{name: "escaped", method: "POST", path: "/fixture/%72pc", want: http.StatusBadRequest},
		{name: "private error", method: "POST", path: "/fixture/rpc", decodeError: errors.New("private credential"), want: http.StatusBadRequest, decodes: 1},
		{name: "public error", method: "POST", path: "/fixture/rpc", decodeError: &sdk.Error{Status: http.StatusUnprocessableEntity, Message: "unsupported input"}, want: http.StatusUnprocessableEntity, decodes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			decodes, finished, reached := 0, 0, false
			adapter := fixtureInbound{decode: func(context.Context, http.Header, []byte) (sdk.Request, error) {
				decodes++
				return sdk.Request{}, test.decodeError
			}, finish: func(error) error { finished++; return nil }}
			server := protocolServer(t, adapter)
			server.requestLimits.body = test.limit
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			server.protocolEntry(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })).ServeHTTP(response, request)
			if reached || response.Code != test.want || decodes != test.decodes || finished != 1 || strings.Contains(response.Body.String(), "private credential") {
				t.Fatalf("reached=%v status=%d decodes=%d Finish=%d: %s", reached, response.Code, decodes, finished, response.Body)
			}
		})
	}
}

func TestProtocolBridgeCancellationReachesProviderAndReleasesKeySlot(t *testing.T) {
	fresh(t)
	started, stopped, abort := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		close(started)
		select {
		case <-request.Context().Done():
		case <-abort:
		}
		close(stopped)
	}))
	t.Cleanup(func() {
		close(abort)
		upstream.CloseClientConnections()
		upstream.Close()
	})
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: "key", Chat: upstream.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	keys, secrets := newCaller(t, "Concurrent client")
	keyCap(t, keys[0].ID, 1)
	var finished atomic.Int64
	var cause error
	adapter := fixtureInbound{finish: func(err error) error {
		finished.Add(1)
		cause = err
		return nil
	}}
	request := httptest.NewRequest(http.MethodPost, adapter.Path(), strings.NewReader("fixture/m1")).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer "+secrets[0])
	server := protocolServer(t, adapter)
	handler := server.Front()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("request never reached provider")
	}
	if lane := server.KeyLanes()[keys[0].ID]; lane.Busy != 1 || lane.Limit != 1 {
		t.Fatalf("normalized inference bypassed key concurrency: %+v", lane)
	}
	cancel()
	for _, channel := range []<-chan struct{}{done, stopped} {
		select {
		case <-channel:
		case <-time.After(15 * time.Second):
			t.Fatal("canceled request remained active")
		}
	}
	if finished.Load() != 1 || !errors.Is(cause, context.Canceled) {
		t.Fatalf("Finish count=%d cause=%v", finished.Load(), cause)
	}
	if lanes := server.KeyLanes(); len(lanes) != 0 {
		t.Fatalf("canceled protocol retained key slots: %+v", lanes)
	}
}
