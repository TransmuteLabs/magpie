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

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestOpenRouterDailyUsageDoesNotHoldGroup(t *testing.T) {
	fresh(t)
	const (
		base       = "https://openrouter.ai/api/v1"
		free       = "vendor/free:free"
		paid       = "vendor/paid"
		providerID = "reportrouter"
	)
	var mu sync.Mutex
	var inferred []string
	var keyHeaders []string
	var redirected bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/credits":
			io.WriteString(w, `{"data":{"total_credits":20,"total_usage":3.5}}`)
		case "GET /api/v1/key":
			mu.Lock()
			keyHeaders = append(keyHeaders, auth)
			mu.Unlock()
			if r.Header.Get("X-Only-Inference") != "" {
				t.Error("inference header reached the account introspection request")
			}
			switch auth {
			case "Bearer sk-c33-empty":
				io.WriteString(w, `{"data":{"usage_daily":15,"free_model_daily_requests":{"used":1004,"limit":1000,"remaining":0}}}`)
			case "Bearer sk-c33-absent":
				io.WriteString(w, `{"data":{"limit":2,"limit_remaining":0,"usage_daily":15,"rate_limit":{"requests":10,"interval":"10s"}}}`)
			case "Bearer sk-c33-partial":
				io.WriteString(w, `{"data":{"free_model_daily_requests":{"used":50,"limit":50}}}`)
			case "Bearer sk-c33-redirect":
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
			default:
				t.Error("unexpected key introspection credential")
				http.Error(w, "not found", http.StatusNotFound)
			}
		case "GET /elsewhere":
			mu.Lock()
			redirected = true
			mu.Unlock()
			io.WriteString(w, `{"data":{"free_model_daily_requests":{"used":50,"limit":50,"remaining":0}}}`)
		case "POST /api/v1/chat/completions":
			var request struct{ Model string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			inferred = append(inferred, request.Model+" "+auth)
			mu.Unlock()
			fmt.Fprintf(w, `{"id":"from-openrouter","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, request.Model)
		default:
			http.Error(w, "unexpected test request", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "openrouter.ai" {
			return nil, fmt.Errorf("blocked non-fixture request to %s", r.URL.Host)
		}
		request := r.Clone(r.Context())
		request.URL.Scheme, request.URL.Host = "http", upstream.Listener.Addr().String()
		return upstream.Client().Transport.RoundTrip(request)
	})
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	if err := provider.Save(provider.Provider{
		ID: providerID, Name: "OpenRouter", Preset: "openrouter", Chat: base,
		Key: "sk-c33-empty", KeyName: "exhausted", Models: []string{free, paid},
		Headers: map[string]string{"X-Only-Inference": "fixture"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(providerID, base, []catalog.Model{{ID: free, Context: 128000}, {ID: paid, Context: 128000}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ id, key string }{
		{"legacyrouter", "sk-c33-absent"}, {"partialrouter", "sk-c33-partial"}, {"redirectrouter", "sk-c33-redirect"},
	} {
		if err := provider.Save(provider.Provider{ID: entry.id, Name: entry.id, Chat: base, Key: entry.key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.Save(provider.Provider{
		ID: "mixedrouter", Name: "Mixed host", Chat: base,
		Responses: "https://fixture.invalid/v1", Key: "sk-c33-mixed",
	}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []provider.Group{
		{Name: "Free First", Routing: provider.Ordered, Members: []string{providerID + "/" + free, providerID + "/" + paid}},
		{Name: "Paid First", Routing: provider.Ordered, Members: []string{providerID + "/" + paid, providerID + "/" + free}},
	} {
		if err := provider.SaveGroup(group); err != nil {
			t.Fatal(err)
		}
	}
	provider.ForgetBalances()
	t.Cleanup(provider.ForgetBalances)
	prefs := settings.Load()
	prefs.QuotaReads = "asked"
	if err := settings.Save(prefs); err != nil {
		t.Fatal(err)
	}
	provider.KeyBalances(context.Background())
	mu.Lock()
	unasked := len(keyHeaders)
	mu.Unlock()
	if unasked != 0 {
		t.Fatalf("unasked key introspections = %d", unasked)
	}
	provider.ForgetBalances()
	start := time.Now().UTC().Format(time.DateOnly)
	cards := provider.KeyBalances(provider.Asked(context.Background()))
	day := time.Now().UTC().Format(time.DateOnly)
	if start != day {
		provider.ForgetBalances()
		cards = provider.KeyBalances(provider.Asked(context.Background()))
	}
	s := New()
	s.client = &http.Client{Transport: transport}
	for _, example := range []struct{ group, want string }{{"free-first", free}, {"paid-first", paid}} {
		body := fmt.Sprintf(`{"model":"group/%s","messages":[{"role":"user","content":"hi"}]}`, example.group)
		if code, reply := postAs(t, s, "", body); code != http.StatusOK || !strings.Contains(reply, example.want) {
			t.Fatalf("%s returned %d, %s", example.group, code, reply)
		}
	}
	mu.Lock()
	models, headers, followed := slices.Clone(inferred), slices.Clone(keyHeaders), redirected
	mu.Unlock()
	if !slices.Equal(models, []string{free + " Bearer sk-c33-empty", paid + " Bearer sk-c33-empty"}) {
		t.Fatalf("actual group dispatch: %q", models)
	}
	if followed {
		t.Fatal("key introspection redirect was followed")
	}
	encoded, err := json.Marshal(cards)
	if err != nil {
		t.Fatal(err)
	}
	var output []struct {
		Provider string
		Balance  string
		Windows  []json.RawMessage
		Daily    *struct {
			Day                    string
			Used, Limit, Remaining int
		} `json:"freeModelDaily"`
	}
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]int, len(output))
	for index, card := range output {
		byID[card.Provider] = index
	}
	for _, id := range []string{providerID, "legacyrouter", "partialrouter", "redirectrouter", "mixedrouter"} {
		index, ok := byID[id]
		if !ok {
			t.Fatalf("missing balance card for %s", id)
		}
		card := output[index]
		if card.Balance != "$16.50" || len(card.Windows) != 0 {
			t.Errorf("%s: expected the existing balance and no routing windows, got %+v", id, card)
		}
		if id != providerID && card.Daily != nil {
			t.Errorf("%s: unknown counter was projected: %+v", id, card.Daily)
		}
	}
	daily := output[byID[providerID]].Daily
	if daily == nil || daily.Day != day || daily.Used != 1004 || daily.Limit != 1000 || daily.Remaining != 0 {
		t.Fatalf("missing or incorrect reported daily counter: %+v", daily)
	}
	slices.Sort(headers)
	if !slices.Equal(headers, []string{"Bearer sk-c33-absent", "Bearer sk-c33-empty", "Bearer sk-c33-partial", "Bearer sk-c33-redirect"}) && start == day {
		t.Fatalf("key introspections = %q", headers)
	}
}
