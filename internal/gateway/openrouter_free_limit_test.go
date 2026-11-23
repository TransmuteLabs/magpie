package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func TestOpenRouterFreeTierKeepsPaidGroupMemberReady(t *testing.T) {
	for _, test := range []struct {
		name, message, source, why string
		wait                       time.Duration
	}{
		{"minute", "Rate limit exceeded: free-models-per-min. ", "openrouter_free_tier_per_minute", failRate, 25 * time.Second},
		{"day", "Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day", "openrouter_free_tier_daily", failQuota, 3 * time.Hour},
		{"day_without_header", "The daily request limit was exceeded", "openrouter_free_tier_daily", failQuota, 0},
		{"legacy_minute", "ORF: Rate limit exceeded: free-models-per-min", "", failRate, 25 * time.Second},
		{"legacy_day", "Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day", "", failQuota, 3 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			opts := settings.Load()
			retries, extraDispatches := 0, 1
			opts.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxRetries: &retries, MaxTotalRetries: &extraDispatches,
			}}
			if err := settings.Save(opts); err != nil {
				t.Fatal(err)
			}

			now := time.Now()
			reset := time.UnixMilli(now.Add(test.wait).UnixMilli())
			if test.wait == 0 {
				reset = now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
			}
			var observed struct {
				sync.Mutex
				tried   []string
				limited bool
			}
			observed.limited = true
			vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					http.NotFound(w, r)
					return
				}
				body, _ := io.ReadAll(r.Body)
				model := modelOf(body)
				observed.Lock()
				observed.tried = append(observed.tried, model)
				limited := observed.limited
				observed.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if limited && (strings.HasSuffix(model, ":free") || model == "openrouter/free") {
					if test.wait != 0 {
						w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
					}
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprintf(w, `{"error":{"message":%q,"code":429,"metadata":{"limit_source":%q}}}`, test.message, test.source)
					return
				}
				fmt.Fprintf(w, `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, model)
			}))
			t.Cleanup(vendor.Close)
			if err := provider.Save(provider.Provider{
				ID: "orf", Name: "OpenRouter", Preset: "openrouter", Key: "k", Chat: vendor.URL + "/v1",
				Models: []string{"x/a:free", "x/b", "x/paid", "openrouter/free"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, changed, err := provider.SetModelID("orf/x/a:free", "first"); err != nil || !changed {
				t.Fatalf("set model ID: changed %v, error %v", changed, err)
			}
			if err := provider.SetUpstreamName("orf/x/b", "x/b:free"); err != nil {
				t.Fatal(err)
			}
			if _, changed, err := provider.SetModelID("orf/x/paid", "paid:free"); err != nil || !changed {
				t.Fatalf("set model ID: changed %v, error %v", changed, err)
			}
			for _, group := range []provider.Group{
				{Name: "Mixed", Members: []string{"orf/first", "orf/x/b", "orf/openrouter/free", "orf/paid:free"}, Routing: provider.Ordered, Patience: provider.PatienceOff},
				{Name: "Free", Members: []string{"orf/x/b", "orf/first"}, Routing: provider.Ordered, Patience: provider.PatienceOff},
				{Name: "FreeRouter", Members: []string{"orf/openrouter/free"}, Routing: provider.Ordered, Patience: provider.PatienceOff},
			} {
				if err := provider.SaveGroup(group); err != nil {
					t.Fatal(err)
				}
			}
			server := New()
			send := func(group string, status int, phrase string) {
				t.Helper()
				code, body := postAs(t, server, "", `{"model":"group/`+group+`","messages":[{"role":"user","content":"hi"}]}`)
				if code != status || !strings.Contains(body, phrase) {
					t.Fatalf("%s: %d %s, want %d with %q", group, code, body, status, phrase)
				}
			}
			assertTried := func(want ...string) {
				t.Helper()
				observed.Lock()
				defer observed.Unlock()
				if !slices.Equal(observed.tried, want) {
					t.Errorf("models sent upstream: %v, want %v", observed.tried, want)
				}
			}

			send("mixed", http.StatusOK, "x/paid")
			assertTried("x/a:free", "x/paid")
			route := server.trace.routes[len(server.trace.routes)-1]
			first := route.Tries[0]
			if first.Fail != test.why || first.Rest == nil || first.Rest.Key == "orf" || first.Rest.Model != "" {
				t.Fatalf("free-tier refusal with vendor reset %s: %+v, rest %+v", reset, first, first.Rest)
			}
			if !first.Rest.Until.Equal(reset) && (test.wait != 0 || !first.Rest.Until.Equal(reset.Add(24*time.Hour)) || time.Now().Before(reset)) {
				t.Fatalf("rest ends at %s, want %s", first.Rest.Until, reset)
			}
			if _, held := RestOf("orf"); held {
				t.Fatal("free-tier refusal rested the whole key")
			}
			send("mixed", http.StatusOK, "x/paid")
			send("free", http.StatusTooManyRequests, "not asked again before")
			send("freerouter", http.StatusTooManyRequests, "not asked again before")
			assertTried("x/a:free", "x/paid", "x/paid")
			freeRouterRoute := server.trace.routes[len(server.trace.routes)-1]
			if len(freeRouterRoute.Order) != 1 || freeRouterRoute.Order[0].Rest == nil || freeRouterRoute.Order[0].Rest.Key != first.Rest.Key {
				t.Fatalf("free router should show the same scoped rest: %+v", freeRouterRoute.Order)
			}

			restingUntil.Lock()
			restingUntil.m[first.Rest.Key] = time.Now().Add(-time.Second)
			restingUntil.Unlock()
			observed.Lock()
			observed.limited = false
			observed.Unlock()
			send("free", http.StatusOK, "x/b:free")
			assertTried("x/a:free", "x/paid", "x/paid", "x/b:free")
		})
	}
}

func TestOpenRouterFreeTierKeepsRequestWireDuringRewire(t *testing.T) {
	const paidWire = "x/b-paid"
	const freeWire = "x/b:free"
	const firstWire = "x/a:free"
	const holdText = "not asked again before"
	for _, test := range []struct {
		name, before, after string
		wantStatus          int
		wantPhrase          string
		wantModels          []string
		nextStatus          int
		nextPhrase          string
		nextModels          []string
	}{
		{"paid_to_free", paidWire, freeWire, http.StatusOK, paidWire, []string{firstWire, paidWire},
			http.StatusTooManyRequests, holdText, []string{firstWire, paidWire}},
		{"free_to_paid", freeWire, paidWire, http.StatusTooManyRequests, holdText, []string{firstWire},
			http.StatusOK, paidWire, []string{firstWire, paidWire}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			opts := settings.Load()
			retries, extraDispatches := 0, 1
			opts.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxRetries: &retries, MaxTotalRetries: &extraDispatches,
			}}
			if err := settings.Save(opts); err != nil {
				t.Fatal(err)
			}

			firstArrived := make(chan struct{})
			releaseFirst := make(chan struct{})
			var firstOnce sync.Once
			var attemptsMu sync.Mutex
			var attempts []string
			vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					http.NotFound(w, r)
					return
				}
				body, _ := io.ReadAll(r.Body)
				model := modelOf(body)
				attemptsMu.Lock()
				attempts = append(attempts, model)
				attemptsMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if model == firstWire {
					firstOnce.Do(func() { close(firstArrived) })
					select {
					case <-releaseFirst:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Retry-After", "300")
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"error":{"message":"Rate limit exceeded: free-models-per-min.","code":429,"metadata":{"limit_source":"openrouter_free_tier_per_minute"}}}`)
					return
				}
				fmt.Fprintf(w, `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, model)
			}))
			t.Cleanup(vendor.Close)
			if err := provider.Save(provider.Provider{
				ID: "orf", Name: "OpenRouter", Preset: "openrouter", Key: "k", Chat: vendor.URL + "/v1",
				Models: []string{firstWire, "x/b"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetUpstreamName("orf/x/b", test.before); err != nil {
				t.Fatal(err)
			}
			if err := provider.SaveGroup(provider.Group{
				Name: "Mixed", Members: []string{"orf/" + firstWire, "orf/x/b"}, Routing: provider.Ordered, Patience: provider.PatienceOff,
			}); err != nil {
				t.Fatal(err)
			}

			server := New()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"group/mixed","messages":[{"role":"user","content":"hi"}]}`))
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.Handler().ServeHTTP(response, req)
			}()
			defer func() {
				cancel()
				<-done
			}()
			select {
			case <-firstArrived:
			case <-done:
				t.Fatalf("group ended before first upstream attempt: %d %s", response.Code, response.Body)
			case <-ctx.Done():
				t.Fatal("first upstream attempt did not arrive:", ctx.Err())
			}
			if err := provider.SetUpstreamName("orf/x/b", test.after); err != nil {
				t.Fatal(err)
			}
			if got := provider.UpstreamNameIn(currentWires(), "orf", "x/b"); got != test.after {
				t.Fatalf("updated wire = %q, want %q", got, test.after)
			}
			close(releaseFirst)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("group did not finish after refusal:", ctx.Err())
			}
			attempted := func() []string {
				attemptsMu.Lock()
				defer attemptsMu.Unlock()
				return slices.Clone(attempts)
			}
			models := attempted()
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantPhrase) || !slices.Equal(models, test.wantModels) {
				t.Fatalf("wire snapshot: status %d, models %v, body %s; want %d, models %v, %q", response.Code, models, response.Body, test.wantStatus, test.wantModels, test.wantPhrase)
			}
			if test.wantStatus == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "" {
				t.Fatal("held response has no Retry-After")
			}
			code, body := postAs(t, server, "", `{"model":"orf/x/b","messages":[{"role":"user","content":"hi"}]}`)
			models = attempted()
			if code != test.nextStatus || !strings.Contains(body, test.nextPhrase) || !slices.Equal(models, test.nextModels) {
				t.Fatalf("next request: status %d, models %v, body %s; want %d, models %v, %q", code, models, body, test.nextStatus, test.nextModels, test.nextPhrase)
			}
		})
	}
}

func TestOpenRouterUnscopedLimitStillRestsKey(t *testing.T) {
	fresh(t)
	fake := &openRouterFreeLimit{
		body: `{"error":{"message":"Rate limit exceeded: free-models-per-min.","code":429,"metadata":{"limit_source":"openrouter_abuse_guard"}}}`,
	}
	vendor := httptest.NewServer(fake)
	t.Cleanup(vendor.Close)
	if err := provider.Save(provider.Provider{
		ID: "orf", Name: "OpenRouter", Preset: "openrouter", Key: "k", Chat: vendor.URL + "/v1",
		Models: []string{"x/a:free", "x/paid"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{
		Name: "Mixed", Members: []string{"orf/x/a:free", "orf/x/paid"}, Routing: provider.Ordered, Patience: provider.PatienceOff,
	}); err != nil {
		t.Fatal(err)
	}
	server := New()
	code, body := postAs(t, server, "", `{"model":"group/mixed","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || !strings.Contains(body, "from b") {
		t.Fatalf("group: %d %s", code, body)
	}
	first := server.trace.routes[len(server.trace.routes)-1].Tries[0]
	if first.Rest == nil || first.Rest.Key != "orf" || first.Rest.Model != "" {
		t.Fatalf("unclassified refusal should rest the key: %+v", first.Rest)
	}
}

func TestOpenRouterFreeTierWithTranslatedSearch(t *testing.T) {
	testOpenRouterFreeTierWithTranslatedTool(t, "web_search")
}

func TestOpenRouterFreeTierWithTranslatedAdvisor(t *testing.T) {
	testOpenRouterFreeTierWithTranslatedTool(t, advisorTool)
}

func testOpenRouterFreeTierWithTranslatedTool(t *testing.T, tool string) {
	t.Helper()
	const (
		firstFree  = "x/a:free"
		secondFree = "x/b:free"
		paid       = "x/paid"
		key        = "c33-search-key"
		advisorID  = "adv/adv-pro"
	)
	for _, test := range []struct {
		name, preset, source string
		scoped               bool
	}{
		{"free_minute", "openrouter", "openrouter_free_tier_per_minute", true},
		{"unknown", "openrouter", "openrouter_abuse_guard", false},
		{"other_vendor", "", "openrouter_free_tier_per_minute", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			opts := settings.Load()
			opts.Searcher = "off"
			retries, extraDispatches := 0, 1
			opts.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxRetries: &retries, MaxTotalRetries: &extraDispatches,
			}}
			if err := settings.Save(opts); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var tried []string
			var secret string
			tools := `{"type":"web_search_20250305","name":"web_search"}`
			if tool == advisorTool {
				adviceCalls := 0
				advisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					adviceCalls++
					mu.Unlock()
					http.Error(w, "unexpected advisor request", http.StatusBadRequest)
				}))
				t.Cleanup(func() {
					advisor.Close()
					mu.Lock()
					defer mu.Unlock()
					if adviceCalls != 0 {
						t.Errorf("advisor received %d requests before any tool call", adviceCalls)
					}
				})
				if err := provider.Save(provider.Provider{
					ID: "adv", Name: "Advisor", Key: "advisor-key", Chat: advisor.URL + "/v1", Models: []string{"adv-pro"},
				}); err != nil {
					t.Fatal(err)
				}
				secret = advisorKey(t, "group/mixed", "group/free", advisorID)
				tools = `{"type":"advisor_20260301","name":"advisor","model":"` + advisorID + `"}`
			} else {
				apis := newSearchAPIs(t)
				if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					apis.mu.Lock()
					defer apis.mu.Unlock()
					if len(apis.asked) != 0 {
						t.Errorf("unexpected search requests: %v", apis.asked)
					}
				})
			}

			reset := time.UnixMilli(time.Now().Add(2 * time.Minute).UnixMilli())
			vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected translated %s request: %s %s", tool, r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var request struct {
					Model  string
					Stream bool
					Tools  []struct {
						Type     string
						Function struct{ Name string }
					}
				}
				err := json.NewDecoder(r.Body).Decode(&request)
				if err != nil || !request.Stream || len(request.Tools) != 1 ||
					request.Tools[0].Type != "function" || request.Tools[0].Function.Name != tool ||
					r.Header.Get("Authorization") != "Bearer "+key {
					t.Errorf("expected translated %s on the same key: request %+v, decode error %v", tool, request, err)
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				tried = append(tried, request.Model)
				mu.Unlock()
				if request.Model == firstFree {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprintf(w, `{"error":{"message":"Rate limit exceeded.","code":429,"metadata":{"limit_source":%q}}}`, test.source)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(
					fmt.Sprintf(`data: {"id":"ok","choices":[{"index":0,"delta":{"role":"assistant","content":%q}}]}`, request.Model),
					`data: {"id":"ok","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
					`data: [DONE]`,
				))
			}))
			t.Cleanup(vendor.Close)
			if err := provider.Save(provider.Provider{
				ID: "orf", Name: "Upstream", Preset: test.preset, Key: key, Chat: vendor.URL + "/v1",
				Models: []string{firstFree, secondFree, paid},
			}); err != nil {
				t.Fatal(err)
			}
			for _, group := range []provider.Group{
				{Name: "Mixed", Members: []string{"orf/" + firstFree, "orf/" + secondFree, "orf/" + paid}, Routing: provider.Ordered, Patience: provider.PatienceOff},
				{Name: "Free", Members: []string{"orf/" + firstFree, "orf/" + secondFree}, Routing: provider.Ordered, Patience: provider.PatienceOff},
			} {
				if err := provider.SaveGroup(group); err != nil {
					t.Fatal(err)
				}
			}
			server := New()
			send := func(group string) (int, string) {
				body := `{"model":"group/` + group + `","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[` + tools + `]}`
				if tool == advisorTool {
					rec := sendAs(t, server, secret, body)
					return rec.Code, rec.Body.String()
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				req.Header.Set("x-api-key", Token)
				req.RemoteAddr = "127.0.0.1:12345"
				rec := httptest.NewRecorder()
				lanGuard(server.Handler()).ServeHTTP(rec, req)
				return rec.Code, rec.Body.String()
			}
			modelsTried := func() []string {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(tried)
			}
			assertTried := func(want ...string) {
				t.Helper()
				if got := modelsTried(); !slices.Equal(got, want) {
					t.Fatalf("translated %s models sent upstream: %v, want %v", tool, got, want)
				}
			}

			code, body := send("mixed")
			route := server.trace.routes[len(server.trace.routes)-1]
			rest := route.Tries[0].Rest
			keyRest := rest != nil && rest.Key == "orf" && rest.Model == "" && !rest.FreeTier
			held429 := code == http.StatusTooManyRequests && strings.Contains(body, "not asked again before")
			if test.scoped && keyRest && held429 && slices.Equal(modelsTried(), []string{firstFree}) {
				t.Fatal("confirmed OpenRouter free-tier refusal rested the whole key and held the paid fallback")
			}
			wantKey := "orf"
			if test.scoped {
				wantKey += "/\x00openrouter-free"
			}
			if rest == nil || rest.FreeTier != test.scoped || rest.Key != wantKey {
				t.Fatalf("translated %s refusal scope: %+v", tool, rest)
			}
			if test.scoped {
				assertTried(firstFree, paid)
				if code != http.StatusOK || textOf(body) != paid {
					t.Fatalf("translated group replied %d %s, want %q", code, body, paid)
				}
				if !rest.Until.Equal(reset) {
					t.Fatalf("free-tier rest ends at %s, want %s", rest.Until, reset)
				}
				code, body = send("free")
				if code != http.StatusTooManyRequests || !strings.Contains(body, "not asked again before") {
					t.Fatalf("free group during scoped rest: %d %s", code, body)
				}
				code, body = send("mixed")
				if code != http.StatusOK || textOf(body) != paid {
					t.Fatalf("mixed group during scoped rest: %d %s", code, body)
				}
				assertTried(firstFree, paid, paid)
			} else {
				if !keyRest || !held429 {
					t.Fatalf("unscoped refusal did not hold the whole key: %d %s, rest %+v", code, body, rest)
				}
				assertTried(firstFree)
			}
		})
	}
}
