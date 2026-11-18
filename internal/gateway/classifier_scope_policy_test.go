package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

type classifierScopeFixture struct {
	server  *Server
	secrets []string
	mu      sync.Mutex
	sends   []string
	started chan string
	release chan struct{}
	once    sync.Once
}

func classifierScopeTarget(t *testing.T) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, body.Model)
	}))
	t.Cleanup(upstream.Close)
	if err := provider.Save(provider.Provider{ID: "scope-target", Name: "Scope target",
		Key: "target", Chat: upstream.URL + "/v1", Models: []string{"default", "chosen"}}); err != nil {
		t.Fatal(err)
	}
}

func classifierScopeKeys(t *testing.T, providerID string, accounts []string) []string {
	t.Helper()
	keys, secrets := newCaller(t, "Scope A", "Scope B")
	for index, account := range accounts {
		if _, err := access.Update("accounts-key", access.Change{Key: keys[index].ID,
			Accounts: []string{providerID + "/" + account}}); err != nil {
			t.Fatal(err)
		}
		if _, err := access.Update("models-key", access.Change{Key: keys[index].ID,
			Models: []string{"group/*"}}); err != nil {
			t.Fatal(err)
		}
	}
	return secrets
}

func classifierScopeGroup(t *testing.T, id, classifier string, accounts []string) {
	t.Helper()
	if err := provider.SaveGroup(provider.Group{ID: id, Name: id, Routing: provider.Ordered,
		Members: []string{"scope-target/default", "scope-target/chosen"}, Accounts: accounts,
		Classifier: classifier, Rules: []provider.Rule{{Use: "scope-target/chosen", Intent: "bug"}}}); err != nil {
		t.Fatal(err)
	}
}

func classifierScopeFront(ctx context.Context, server *Server, secret, group, session, text string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"model": "group/" + group,
		"messages": []map[string]string{{"role": "user", "content": text}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-session-id", session)
	reply := httptest.NewRecorder()
	server.Front().ServeHTTP(reply, request)
	return reply
}

func newClassifierScopeFixture(t *testing.T, barrier bool) *classifierScopeFixture {
	t.Helper()
	fresh(t)
	resetClassified(t)
	fixture := &classifierScopeFixture{started: make(chan string, 8), release: make(chan struct{})}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			io.WriteString(writer, `{"data":[{"id":"judge"}]}`)
			return
		}
		io.Copy(io.Discard, request.Body)
		account := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		fixture.mu.Lock()
		fixture.sends = append(fixture.sends, account)
		fixture.mu.Unlock()
		fixture.started <- account
		if barrier && account == "scope-a" {
			select {
			case <-fixture.release:
			case <-request.Context().Done():
				return
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		if account == "scope-a" {
			writer.WriteHeader(http.StatusBadRequest)
			io.WriteString(writer, `{"error":{"message":"scope A classifier refused"}}`)
			return
		}
		io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"1"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { fixture.once.Do(func() { close(fixture.release) }) })
	if err := provider.Save(provider.Provider{ID: "scope-judge", Name: "Scope judge",
		Key: "scope-a", Keys: []provider.KeyAccount{{Key: "scope-b"}}, Routing: provider.Ordered,
		Chat: upstream.URL + "/v1", Models: []string{"judge"}}); err != nil {
		t.Fatal(err)
	}
	classifierScopeTarget(t)
	classifierScopeGroup(t, "scope", "scope-judge/judge", nil)
	fixture.secrets = classifierScopeKeys(t, "scope-judge", []string{
		provider.KeyID("scope-a"), provider.KeyID("scope-b"),
	})
	current := settings.Load()
	zero := 0
	current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
		MaxTotalRetries: &zero, MaxRetries: &zero,
	}}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	fixture.server = New()
	t.Cleanup(fixture.server.idle.stop)
	return fixture
}

func (fixture *classifierScopeFixture) took() []string {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return slices.Clone(fixture.sends)
}

func TestClassifierScopeEffectiveCallerThroughFront(t *testing.T) {
	fixture := newClassifierScopeFixture(t, false)
	first := classifierScopeFront(t.Context(), fixture.server, fixture.secrets[0], "scope", "a-first", "fix first bug")
	if first.Code != 200 || !strings.Contains(first.Body.String(), "default") ||
		!slices.Equal(fixture.took(), []string{"scope-a"}) {
		t.Fatalf("A fixture: %d %s sends=%v", first.Code, first.Body, fixture.took())
	}
	second := classifierScopeFront(t.Context(), fixture.server, fixture.secrets[1], "scope", "b-first", "fix second bug")
	if second.Code != 200 || !strings.Contains(second.Body.String(), "chosen") ||
		!slices.Equal(fixture.took(), []string{"scope-a", "scope-b"}) {
		t.Errorf("A failure crossed effective caller accounts: %d %s sends=%v", second.Code, second.Body, fixture.took())
	}
	rest := classifierScopeFront(t.Context(), fixture.server, fixture.secrets[0], "scope", "a-rest", "fix third bug")
	route := lastRoute(fixture.server)
	if rest.Code != 200 || route.Rule == nil || route.Rule.Classified == nil ||
		!strings.Contains(route.Rule.Classified.Error, "not asked again") ||
		!slices.Equal(fixture.took(), []string{"scope-a", "scope-b"}) {
		t.Errorf("same-account classifier rest lost: %d %s route=%+v sends=%v", rest.Code, rest.Body, route.Rule, fixture.took())
	}
	cached := classifierScopeFront(t.Context(), fixture.server, fixture.secrets[0], "scope", "a-success-cache", "fix second bug")
	route = lastRoute(fixture.server)
	if cached.Code != 200 || !strings.Contains(cached.Body.String(), "chosen") || route.Rule == nil ||
		route.Rule.Classified == nil || !route.Rule.Classified.Cached ||
		!slices.Equal(fixture.took(), []string{"scope-a", "scope-b"}) {
		t.Errorf("successful answer stopped sharing by question: %d %s route=%+v sends=%v", cached.Code, cached.Body, route.Rule, fixture.took())
	}
}

func TestClassifierScopeDisjointInflightThroughFront(t *testing.T) {
	fixture := newClassifierScopeFixture(t, true)
	classifierScopeGroup(t, "scope-a", "scope-judge/judge", []string{"scope-judge/" + provider.KeyID("scope-a")})
	classifierScopeGroup(t, "scope-b", "scope-judge/judge", []string{"scope-judge/" + provider.KeyID("scope-b")})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		fixture.once.Do(func() { close(fixture.release) })
		workers.Wait()
	})
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		first <- classifierScopeFront(ctx, fixture.server, fixture.secrets[0], "scope-a", "first", "same inflight question")
	}()
	select {
	case account := <-fixture.started:
		if account != "scope-a" {
			t.Fatalf("first request escaped A: %s", account)
		}
	case <-ctx.Done():
		t.Fatal("A did not reach the classifier before the bound")
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		second <- classifierScopeFront(ctx, fixture.server, fixture.secrets[1], "scope-b", "second", "same inflight question")
	}()
	wait, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	select {
	case account := <-fixture.started:
		if account != "scope-b" {
			t.Errorf("disjoint request escaped B: %s", account)
		}
	case <-wait.Done():
		t.Error("disjoint B classifier did not dispatch before A was released")
	}
	fixture.once.Do(func() { close(fixture.release) })
	for index, result := range []<-chan *httptest.ResponseRecorder{first, second} {
		select {
		case reply := <-result:
			if reply.Code != 200 || index == 1 && !strings.Contains(reply.Body.String(), "chosen") {
				t.Errorf("inflight caller %d inherited another scope's failure: %d %s", index, reply.Code, reply.Body)
			}
		case <-ctx.Done():
			t.Fatal("ordinary Front request did not join before its bound")
		}
	}
	if !slices.Equal(fixture.took(), []string{"scope-a", "scope-b"}) {
		t.Fatalf("disjoint scopes sent unexpected classifier calls: %v", fixture.took())
	}
}

func TestClassifierScopeSDKTerminalThroughFront(t *testing.T) {
	directory := buildDecisionFixture(t, "exec")
	manager, id := installDecisionFixture(t, directory, "accepted error")
	resetClassified(t)
	classifierScopeTarget(t)
	classifierScopeGroup(t, "scope-sdk", id+"/decision", nil)
	secrets := classifierScopeKeys(t, id, []string{"alice", "bob"})
	server := New()
	t.Cleanup(server.idle.stop)
	first := classifierScopeFront(t.Context(), server, secrets[0], "scope-sdk", "sdk-a", "fix accepted A bug")
	if first.Code != 409 || first.Header().Get("X-Should-Retry") != "false" ||
		first.Header().Get("X-Magpie-Operation") == "" || len(decisionExecutions(t, manager, "alice")) != 1 ||
		len(decisionExecutions(t, manager, "bob")) != 0 {
		t.Fatalf("A terminal fixture lost fence/receipt: %d %v %s", first.Code, first.Header(), first.Body)
	}
	second := classifierScopeFront(t.Context(), server, secrets[1], "scope-sdk", "sdk-b", "fix independent B bug")
	if second.Code != 200 || !strings.Contains(second.Body.String(), "chosen") ||
		second.Header().Get("X-Magpie-Operation") != "" || len(decisionExecutions(t, manager, "bob")) == 0 {
		t.Errorf("A terminal fence crossed into B: %d %v %s bob=%d", second.Code, second.Header(), second.Body,
			len(decisionExecutions(t, manager, "bob")))
	}
	rest := classifierScopeFront(t.Context(), server, secrets[0], "scope-sdk", "sdk-a-rest", "fix resting A bug")
	if rest.Code != 409 || rest.Header().Get("X-Should-Retry") != "false" ||
		rest.Header().Get("X-Magpie-Operation") != "" || len(decisionExecutions(t, manager, "alice")) != 1 {
		t.Errorf("resting A replayed or borrowed its prior receipt: %d %v %s", rest.Code, rest.Header(), rest.Body)
	}
}
