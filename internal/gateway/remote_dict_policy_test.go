package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

func remoteDictPolicyRoot(t *testing.T, total int) context.Context {
	t.Helper()
	zero, repeats := 0, 1
	current := settings.Load()
	current.RequestPolicy = &requestpolicy.Config{
		Defaults: requestpolicy.Options{
			MaxTotalRetries: &total, MaxRetries: &repeats, BaseDelayMS: &zero,
		},
		Providers: map[string]requestpolicy.Options{"fake": {MaxRetries: &zero}},
	}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	return withRequestPolicy(t.Context())
}

func remoteDictPolicyFront(server *Server, ctx context.Context, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:5555"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	return response
}

func TestRemoteDictCallbackSharesRootBudget(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			fresh(t)
			_, vendor, hop := twoMagpies(t)
			server := New()
			t.Cleanup(server.idle.stop)
			chat := newAgentChat(t, 81)
			if response := remoteDictPolicyFront(server, remoteDictPolicyRoot(t, total), chat.turn(0)); response.Code != http.StatusOK {
				t.Fatalf("warmup: status=%d body=%s", response.Code, response.Body.String())
			}
			if hop.n() != 1 || vendor.n() != 1 {
				t.Fatalf("warmup: remote=%d vendor=%d", hop.n(), vendor.n())
			}
			heldDicts = newKeptBodies()
			ctx := remoteDictPolicyRoot(t, total)
			body := chat.turn(1)
			response := remoteDictPolicyFront(server, ctx, body)
			remoteSends, vendorSends := hop.n()-1, vendor.n()-1
			used, selected := requestpolicy.FromContext(ctx).Counts("office")
			t.Logf("total=%d status=%d remote=%d vendor=%d root=%d provider=%d", total, response.Code, remoteSends, vendorSends, used, selected)
			if hop.header(1).Get("Content-Encoding") != "dcz" {
				t.Fatal("target did not exercise a dictionary miss")
			}
			if remoteSends != total+1 || vendorSends != total {
				t.Errorf("callback bypassed dispatch allowance: remote=%d want=%d vendor=%d want=%d", remoteSends, total+1, vendorSends, total)
			}
			if used != total || selected != total {
				t.Errorf("callback escaped root/provider accounting: root=%d provider=%d want=%d", used, selected, total)
			}
			if (response.Code == http.StatusOK) != (total == 1) {
				t.Errorf("callback outcome: status=%d total=%d", response.Code, total)
			}
			if total == 1 && remoteSends == 2 && vendorSends == 1 {
				if hop.header(2).Get("Content-Encoding") != "" {
					t.Error("permitted resend was not a whole body")
				}
				sameMessages(t, []byte(vendor.raw(1)), body)
			}
		})
	}
}

func TestRemoteDictCallbackRetainsCancellationAtMiss(t *testing.T) {
	fresh(t)
	_, vendor, hop := twoMagpies(t)
	server := New()
	t.Cleanup(server.idle.stop)
	chat := newAgentChat(t, 82)
	if response := remoteDictPolicyFront(server, remoteDictPolicyRoot(t, 1), chat.turn(0)); response.Code != http.StatusOK {
		t.Fatalf("warmup: status=%d body=%s", response.Code, response.Body.String())
	}
	if hop.n() != 1 || vendor.n() != 1 {
		t.Fatalf("warmup: remote=%d vendor=%d", hop.n(), vendor.n())
	}
	heldDicts = newKeptBodies()
	ctx, cancel := context.WithCancel(remoteDictPolicyRoot(t, 1))
	defer cancel()
	transport := server.client.Transport
	misses := 0
	server.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		if response != nil && response.Header.Get(DictMissHeader) != "" {
			misses++
			cancel()
		}
		return response, err
	})
	response := remoteDictPolicyFront(server, ctx, chat.turn(1))
	used, selected := requestpolicy.FromContext(ctx).Counts("office")
	t.Logf("status=%d remote=%d vendor=%d misses=%d root=%d provider=%d canceled=%v", response.Code, hop.n()-1, vendor.n()-1, misses, used, selected, ctx.Err())
	if misses != 1 || ctx.Err() != context.Canceled || hop.header(1).Get("Content-Encoding") != "dcz" {
		t.Fatal("did not cancel at the actual dictionary-miss boundary")
	}
	if hop.n() != 2 || vendor.n() != 1 || used != 0 || selected != 0 {
		t.Errorf("canceled callback sent or charged work: status=%d remote=%d vendor=%d root=%d provider=%d", response.Code, hop.n()-1, vendor.n()-1, used, selected)
	}
	calls := server.Recent()
	if len(calls) != 2 || calls[0].Status != usage.StatusCanceled || !calls[0].Canceled || response.Body.Len() != 0 {
		t.Errorf("canceled callback lost cancellation outcome: calls=%+v body=%s", calls, response.Body.String())
	}
}

func TestRemoteDictCallbackSharesOuterFallbackBudget(t *testing.T) {
	for _, test := range []struct {
		name   string
		total  int
		cancel bool
	}{
		{name: "total1", total: 1},
		{name: "total2", total: 2},
		{name: "cancelBeforeFallback", total: 2, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			_, vendor, hop := twoMagpies(t)
			var refuse atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				vendor.add(request)
				w.Header().Set("Content-Type", "application/json")
				if refuse.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					io.WriteString(w, `{"error":{"type":"overloaded_error","message":"temporarily unavailable"}}`)
					return
				}
				io.WriteString(w, `{"id":"warm","type":"message","role":"assistant","model":"m1","content":[{"type":"text","text":"warmup"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
			}))
			t.Cleanup(upstream.Close)
			selected, err := provider.Find("fake")
			if err != nil {
				t.Fatal(err)
			}
			selected.Anthropic, selected.Patience = upstream.URL, provider.PatienceOff
			if err := provider.Save(*selected); err != nil {
				t.Fatal(err)
			}
			spare := &hopLog{}
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				spare.add(request)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"spare","type":"message","role":"assistant","model":"m1","content":[{"type":"text","text":"from spare"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
			}))
			t.Cleanup(backup.Close)
			if err := provider.Save(provider.Provider{ID: "spare", Name: "Spare", Key: "spare-key", Models: []string{"m1"}, Anthropic: backup.URL, Patience: provider.PatienceOff}); err != nil {
				t.Fatal(err)
			}
			office, err := provider.Find("office")
			if err != nil {
				t.Fatal(err)
			}
			office.Fallback, office.Patience = []string{"spare/m1"}, provider.PatienceOff
			if err := provider.Save(*office); err != nil {
				t.Fatal(err)
			}
			server := New()
			t.Cleanup(server.idle.stop)
			chat := newAgentChat(t, 83)
			if response := remoteDictPolicyFront(server, remoteDictPolicyRoot(t, test.total), chat.turn(0)); response.Code != http.StatusOK {
				t.Fatalf("warmup: status=%d body=%s", response.Code, response.Body.String())
			}
			if hop.n() != 1 || vendor.n() != 1 || spare.n() != 0 {
				t.Fatalf("warmup: remote=%d vendor=%d spare=%d", hop.n(), vendor.n(), spare.n())
			}
			heldDicts = newKeptBodies()
			refuse.Store(true)
			ctx, cancel := context.WithCancel(remoteDictPolicyRoot(t, test.total))
			defer cancel()
			canceled := false
			if test.cancel {
				transport := server.client.Transport
				server.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
					response, err := transport.RoundTrip(request)
					if response != nil && response.StatusCode == http.StatusServiceUnavailable {
						canceled = true
						cancel()
					}
					return response, err
				})
			}
			response := remoteDictPolicyFront(server, ctx, chat.turn(1))
			used, officeUsed := requestpolicy.FromContext(ctx).Counts("office")
			_, spareUsed := requestpolicy.FromContext(ctx).Counts("spare")
			t.Logf("total=%d cancel=%v status=%d remote=%d vendor=%d spare=%d root=%d office=%d spareUsed=%d", test.total, test.cancel, response.Code, hop.n()-1, vendor.n()-1, spare.n(), used, officeUsed, spareUsed)
			if hop.n() != 3 || vendor.n() != 2 || hop.header(1).Get("Content-Encoding") != "dcz" || hop.header(2).Get("Content-Encoding") != "" {
				t.Fatalf("target did not exercise miss then whole-body vendor refusal: remote=%d vendor=%d", hop.n()-1, vendor.n()-1)
			}
			wantSpare := 0
			if test.total == 2 && !test.cancel {
				wantSpare = 1
			}
			if spare.n() != wantSpare {
				t.Errorf("outer fallback escaped callback budget/cancel: spare=%d want=%d", spare.n(), wantSpare)
			}
			if used != 1+wantSpare || officeUsed != 1 || spareUsed != wantSpare {
				t.Errorf("callback/fallback accounting: root=%d office=%d spare=%d want=(%d,1,%d)", used, officeUsed, spareUsed, 1+wantSpare, wantSpare)
			}
			if test.cancel {
				calls := server.Recent()
				if !canceled || ctx.Err() != context.Canceled || len(calls) != 2 || calls[0].Status != usage.StatusCanceled || !calls[0].Canceled || response.Body.Len() != 0 {
					t.Errorf("outer fallback lost caller cancellation: canceled=%v err=%v calls=%+v body=%s", canceled, ctx.Err(), calls, response.Body.String())
				}
			} else if (response.Code == http.StatusOK) != (wantSpare == 1) || wantSpare == 1 && !strings.Contains(response.Body.String(), "from spare") {
				t.Errorf("outer fallback outcome: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
