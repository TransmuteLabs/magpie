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

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
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

func TestOpenRouterFreeTierDuringAdvisorCall(t *testing.T) {
	const (
		executor       = "c33-executor"
		executorKey    = "c33-executor-key"
		executorKeyID  = "9825af56db"
		executorID     = "executor/" + executor
		firstFree      = "x/advisor-a:free"
		secondFree     = "x/advisor-b:free"
		paid           = "x/advisor-paid"
		vendorKey      = "c33-openrouter-key"
		vendorKeyID    = "86289b5415"
		callerAgent    = "claude-cli"
		advisorGroup   = "group/advisor"
		advice         = "Check the paid fallback before shipping."
		advisorErrCode = "too_many_requests"
	)
	writeStream := func(w http.ResponseWriter, model, text, tool, args string, input, output int) {
		w.Header().Set("Content-Type", "text/event-stream")
		response := strings.ReplaceAll(chatSSE(text, tool, args), `"model":"m"`, `"model":`+jsonStr(model))
		counts := fmt.Sprintf(`data: {"id":"c","object":"chat.completion.chunk","model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			model, input, output, input+output)
		io.WriteString(w, strings.Replace(response, "data: [DONE]", counts+"\n\ndata: [DONE]", 1))
	}
	for _, test := range []struct {
		name, source string
		scoped       bool
	}{
		{"free_minute", "openrouter_free_tier_per_minute", true},
		{"unknown", "openrouter_abuse_guard", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			opts := settings.Load()
			retries, extraDispatches := 0, 2
			opts.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxRetries: &retries, MaxTotalRetries: &extraDispatches,
			}}
			if err := settings.Save(opts); err != nil {
				t.Fatal(err)
			}

			reset := time.UnixMilli(time.Now().Add(2 * time.Minute).UnixMilli())
			var mu sync.Mutex
			var tried []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					http.NotFound(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				request := gjson.ParseBytes(body)
				model := request.Get("model").String()
				mu.Lock()
				tried = append(tried, model)
				mu.Unlock()
				key := vendorKey
				if model == executor {
					key = executorKey
				}
				if err != nil || !request.Get("stream").Bool() || r.Header.Get("Authorization") != "Bearer "+key {
					t.Errorf("invalid upstream request for %q: matched key=%v, stream=%v, read error %v",
						model, r.Header.Get("Authorization") == "Bearer "+key, request.Get("stream").Bool(), err)
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}

				switch model {
				case executor:
					messages := request.Get("messages").Array()
					if len(messages) > 1 && messages[len(messages)-1].Get("role").String() == "tool" {
						call, result := messages[len(messages)-2].Get("tool_calls.0"), messages[len(messages)-1]
						want := advice
						if !test.scoped {
							want = "The advisor failed: " + advisorErrCode
						}
						if id := call.Get("id").String(); id == "" || id != result.Get("tool_call_id").String() ||
							call.Get("function.name").String() != advisorTool || !strings.Contains(result.Get("content").Raw, want) {
							t.Errorf("the executor did not receive its advisor result: %s", body)
						}
						writeStream(w, executor, "Done.", "", "", 17, 5)
						return
					}
					if !slices.ContainsFunc(request.Get("tools").Array(), func(tool gjson.Result) bool {
						return tool.Get("function.name").String() == advisorTool
					}) {
						t.Errorf("the executor was not offered advisor(): %s", body)
						http.Error(w, "advisor tool not offered", http.StatusBadRequest)
						return
					}
					writeStream(w, executor, "Let me ask.", advisorTool, "{}", 11, 3)
				case firstFree, paid:
					if request.Get("tool_choice").String() != "none" || !strings.Contains(string(body), "Let me ask.") {
						t.Errorf("the advisor did not receive the consultation: %s", body)
					}
					if model == paid {
						writeStream(w, paid, advice, "", "", 41, 7)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprintf(w, `{"error":{"message":"Rate limit exceeded.","code":429,"metadata":{"limit_source":%q}}}`, test.source)
				default:
					t.Errorf("unexpected upstream model %q", model)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)
			for _, p := range []provider.Provider{
				{ID: "executor", Name: "Executor", Key: executorKey, Chat: upstream.URL + "/v1", Models: []string{executor}},
				{ID: "orf", Name: "OpenRouter", Preset: "openrouter", Key: vendorKey, Chat: upstream.URL + "/v1", Models: []string{firstFree, secondFree, paid}},
			} {
				if err := provider.Save(p); err != nil {
					t.Fatal(err)
				}
			}
			if err := provider.SaveGroup(provider.Group{
				Name: "Advisor", Members: []string{"orf/" + firstFree, "orf/" + secondFree, "orf/" + paid},
				Routing: provider.Ordered, Patience: provider.PatienceOff,
			}); err != nil {
				t.Fatal(err)
			}
			keys, secrets := newCaller(t, "C33 caller")
			if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{executorID, advisorGroup}}); err != nil {
				t.Fatal(err)
			}
			server := New()
			body := strings.Replace(advisorTurn(executorID, advisorGroup), `"stream":true`, `"stream":false`, 1)
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages?beta=true", strings.NewReader(body))
			request.RemoteAddr = "127.0.0.1:50001"
			request.Header.Set("User-Agent", "claude-cli/2.1.316 (external, cli)")
			request.Header.Set("Authorization", "Bearer "+secrets[0])
			reply := httptest.NewRecorder()
			server.Front().ServeHTTP(reply, request)
			result := gjson.ParseBytes(reply.Body.Bytes())
			blocks := result.Get("content").Array()
			if reply.Code != http.StatusOK || result.Get("stop_reason").String() != "end_turn" || len(blocks) != 4 ||
				strings.TrimSpace(blocks[3].Get("text").String()) != "Done." {
				t.Fatalf("executor did not finish: %d %s", reply.Code, reply.Body)
			}
			call, answer := blocks[1], blocks[2]
			id := call.Get("id").String()
			if call.Get("type").String() != "server_tool_use" || call.Get("name").String() != advisorTool ||
				!strings.HasPrefix(id, "srvtoolu_") || answer.Get("type").String() != "advisor_tool_result" ||
				answer.Get("tool_use_id").String() != id {
				t.Fatalf("advisor call and result do not pair: %s", reply.Body)
			}

			want := []string{executor, firstFree, executor}
			advisorStatus, restKey := http.StatusTooManyRequests, "orf"
			if test.scoped {
				want = []string{executor, firstFree, paid, executor}
				advisorStatus, restKey = http.StatusOK, "orf/\x00openrouter-free"
				if answer.Get("content.type").String() != "advisor_result" || answer.Get("content.text").String() != advice {
					t.Fatalf("advisor did not return the paid fallback's advice: %s", answer.Raw)
				}
			} else if answer.Get("content.type").String() != "advisor_tool_result_error" || answer.Get("content.error_code").String() != advisorErrCode {
				t.Fatalf("unscoped refusal did not reach the executor as an advisor error: %s", answer.Raw)
			}
			mu.Lock()
			got := slices.Clone(tried)
			mu.Unlock()
			if !slices.Equal(got, want) {
				t.Fatalf("upstream order %v, want %v", got, want)
			}

			routes := server.Trace(t.Context(), 0, 0).Routes
			if len(routes) != 2 {
				t.Fatalf("expected executor and advisor routes, got %+v", routes)
			}
			executed, consulted := routes[0], routes[1]
			if !executed.Done || executed.Model != executorID || executed.Status != http.StatusOK ||
				!consulted.Done || consulted.Model != advisorGroup || consulted.Status != advisorStatus ||
				executed.CallerKey != keys[0].ID || consulted.CallerKey != keys[0].ID ||
				executed.Agent != callerAgent || consulted.Agent != callerAgent {
				t.Fatalf("executor and advisor route attribution: %+v", routes)
			}
			if len(consulted.Tries) == 0 {
				t.Fatal("advisor has no recorded attempts")
			}
			first := consulted.Tries[0]
			if first.Model != firstFree || first.Status != http.StatusTooManyRequests ||
				first.Rest == nil || first.Rest.Key != restKey || first.Rest.FreeTier != test.scoped {
				t.Fatalf("advisor's first refusal has the wrong scope: %+v, rest %+v", first, first.Rest)
			}
			if _, held := RestOf("orf"); held == test.scoped {
				t.Fatalf("whole key held=%v after advisor's %s refusal", held, test.name)
			}
			if test.scoped && (!first.Rest.Until.Equal(reset) || !slices.ContainsFunc(consulted.Tries, func(attempt Try) bool {
				return attempt.ID == "orf" && attempt.Model == paid && attempt.Done && attempt.Status == http.StatusOK
			})) {
				t.Fatalf("advisor did not route to paid during the free rest: %+v, rest %+v", consulted, first.Rest)
			}

			var executorRows, advisorRows []usage.Record
			for _, row := range usage.Load(time.Time{}) {
				if row.RouteID == executed.ID {
					executorRows = append(executorRows, row)
				}
				if row.RouteID == consulted.ID {
					advisorRows = append(advisorRows, row)
				}
			}
			if len(executorRows) != 1 || executorRows[0].CallerKeyID != keys[0].ID ||
				executorRows[0].ProviderKeyID != executorKeyID || executorRows[0].Agent != callerAgent ||
				executorRows[0].Provider != "executor" || executorRows[0].Model != executor ||
				executorRows[0].Input != 28 || executorRows[0].Output != 8 || executorRows[0].Status != http.StatusOK {
				t.Fatalf("executor Usage rows: %+v", executorRows)
			}
			if test.scoped {
				if len(advisorRows) != 1 || advisorRows[0].CallerKeyID != keys[0].ID ||
					advisorRows[0].ProviderKeyID != vendorKeyID || advisorRows[0].Agent != callerAgent ||
					advisorRows[0].Provider != "orf" ||
					advisorRows[0].Requested != advisorGroup || advisorRows[0].Model != paid || advisorRows[0].Served != paid ||
					advisorRows[0].Input != 41 || advisorRows[0].Output != 7 || advisorRows[0].Status != http.StatusOK {
					t.Fatalf("paid advisor Usage rows: %+v", advisorRows)
				}
			} else if slices.ContainsFunc(advisorRows, func(row usage.Record) bool {
				return row.CallerKeyID != keys[0].ID || row.Agent != callerAgent || row.Status == http.StatusOK ||
					row.Input+row.Output+row.CacheRead+row.CacheWrite != 0
			}) {
				t.Fatalf("unexpected Usage for the unscoped advisor refusal: %+v", advisorRows)
			}
		})
	}
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

func TestOpenRouterFreeTierPluginProviderUsesVendorPreset(t *testing.T) {
	fresh(t)
	pluginProvider := provider.PluginAtForTest("plugin-or", "openrouter-plugin", "https://openrouter.ai/api/v1")
	if pluginProvider.Preset != "" || pluginProvider.VendorPreset() != "openrouter" {
		t.Fatalf("unexpected plugin provider preset=%q vendorPreset=%q", pluginProvider.Preset, pluginProvider.VendorPreset())
	}

	freeCand := candidate{p: pluginProvider, model: "x/a:free", rest: pluginProvider.ID}
	routerCand := candidate{p: pluginProvider, model: "openrouter/free", rest: pluginProvider.ID}
	paidCand := candidate{p: pluginProvider, model: "x/paid", rest: pluginProvider.ID}

	if !freeCand.openRouterFreeLimited(openRouterFreeMinute, http.StatusTooManyRequests) ||
		!routerCand.openRouterFreeLimited(openRouterFreeDay, http.StatusTooManyRequests) ||
		paidCand.openRouterFreeLimited(openRouterFreeMinute, http.StatusTooManyRequests) {
		t.Fatalf("openRouterFreeLimited did not match plugin OpenRouter provider via VendorPreset")
	}

	reset := time.UnixMilli(time.Now().Add(45 * time.Second).UnixMilli())
	header := http.Header{}
	header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
	body := []byte(`{"error":{"message":"Rate limit exceeded: free-models-per-min.","code":429,"metadata":{"limit_source":"openrouter_free_tier_per_minute"}}}`)
	server := New()
	recorded := server.restAfter(freeCand, http.StatusTooManyRequests, header, body)
	if recorded.Why != failRate || !recorded.Until.Equal(reset) {
		t.Fatalf("plugin free-tier rest = %+v, want rate rest until %s", recorded, reset)
	}
	if _, wholeResting := restOf(freeCand.restKey()); wholeResting {
		t.Fatal("plugin OpenRouter free-tier limit rested the whole provider")
	}
	if got, resting := routerCand.openRouterFreeRest(); !resting || !got.Until.Equal(reset) {
		t.Fatalf("plugin openrouter/free rest = %+v (resting=%v), want until %s", got, resting, reset)
	}
	if _, paidResting := paidCand.openRouterFreeRest(); paidResting {
		t.Fatal("plugin OpenRouter free-tier limit held a paid model")
	}

	wires := func() map[string]string {
		return map[string]string{pluginProvider.ID + "/x/alias": "x/alias:free"}
	}
	out, _, _, _ := perKeyBarredWithWires(pluginProvider, "x/alias", provider.Chat, wires)
	if len(out) != 1 {
		t.Fatalf("perKeyBarredWithWires returned %d candidates, want 1", len(out))
	}
	if got, resting := out[0].openRouterFreeRest(); !resting || !got.Until.Equal(reset) {
		t.Fatalf("plugin aliased free model did not keep wires or read free-tier rest: %+v (resting=%v)", got, resting)
	}
}
