package gateway

import (
	"context"
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
