package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func requestPolicyFront(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	server := New()
	t.Cleanup(server.idle.stop)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	return response
}

func TestSharedPolicySelectedModelLimitsReachOrdinaryRouting(t *testing.T) {
	for _, limit := range []string{"maxTotalRetries", "maxRetries", "otherModel"} {
		t.Run(limit, func(t *testing.T) {
			fresh(t)
			zero, total := 0, 8
			model, options := "m", requestpolicy.Options{MaxTotalRetries: &zero}
			if limit == "maxRetries" {
				options = requestpolicy.Options{MaxRetries: &zero}
			} else if limit == "otherModel" {
				model = "other"
			}
			current := settings.Load()
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				BaseDelayMS: &zero, MaxTotalRetries: &total,
				Models: map[string]requestpolicy.Options{model: options},
			}}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			first := &scripted{replies: []reply{{503, "", `{"error":{"message":"temporarily unavailable"}}`}, {200, "", chatOK}}}
			second := &scripted{replies: []reply{{200, "", chatOK}}}
			scriptedOn(t, "first", provider.Chat, first)
			scriptedOn(t, "second", provider.Chat, second)
			members := []string{"first/m", "second/m"}
			if limit == "maxRetries" {
				members = members[:1]
			}
			if err := provider.SaveGroup(provider.Group{ID: "model-policy", Name: "Model policy", Members: members, Routing: "order"}); err != nil {
				t.Fatal(err)
			}
			status := requestPolicyFront(t, `{"model":"model-policy","messages":[{"role":"user","content":"hi"}]}`).Code
			switch limit {
			case "maxTotalRetries":
				if status < 400 || first.n != 1 || second.n != 0 {
					t.Fatalf("model total limit bypassed: status=%d sends=(%d,%d)", status, first.n, second.n)
				}
			case "maxRetries":
				if status < 400 || first.n != 1 || second.n != 0 {
					t.Fatalf("model repeat limit bypassed: status=%d sends=(%d,%d)", status, first.n, second.n)
				}
			case "otherModel":
				if status != 200 || first.n != 1 || second.n != 1 {
					t.Fatalf("unrelated model override changed routing: status=%d sends=(%d,%d)", status, first.n, second.n)
				}
			}
		})
	}
}

func TestSharedPolicyUnaffordableWaitUsesReadySibling(t *testing.T) {
	shortWait, maxDelay, elapsed, total := 45000, 60000, 30000, 1
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = requestpolicy.WithConfig(ctx, &requestpolicy.Config{Defaults: requestpolicy.Options{
		ShortWaitMS: &shortWait, MaxDelayMS: &maxDelay, MaxElapsedMS: &elapsed, MaxTotalRetries: &total,
	}})
	selected := candidateOf(provider.Provider{ID: "fixture"}, "free")
	policy, err := requestpolicy.FromContext(ctx).PolicyFor("fixture", "free", "", requestpolicy.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := requestpolicy.FromContext(ctx).Dispatch(ctx, policy); err != nil {
		t.Fatal(err)
	}
	refusal := &requestpolicy.Refusal{
		Classification: requestpolicy.Classification{Class: requestpolicy.Quota, Scope: "model", Rule: "fixture.free"},
		Status:         429, Step: "inference", NoEffect: true, NextAllowed: time.Now().Add(40 * time.Second),
	}
	decision := resolveFinalPolicy(ctx, selected, refusal, true, func(context.Context, time.Duration) error {
		t.Fatal("wait exceeds elapsed request budget")
		return nil
	})
	if decision.Action != requestpolicy.ActionNext || !decision.NextAllowed.Equal(refusal.NextAllowed) {
		t.Fatalf("unaffordable wait stopped a ready sibling: %+v", decision)
	}
	if used, _ := requestpolicy.FromContext(ctx).Counts("fixture"); used != 0 {
		t.Fatal("failed wait admission spent the sibling permit", used)
	}
}

func TestSharedPolicyLegacyEmptyBusyEnvelopeStillRetries(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"error":{}}`,
		`{"error":{"code":"busy"}}`,
		`{"error":{"code":null,"type":"server_error","message":"temporarily unavailable"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			fresh(t)
			scaled(t, time.Second)
			upstream := &scripted{replies: []reply{{http.StatusServiceUnavailable, "", body}, {200, "", chatOK}}}
			scriptedOn(t, "legacy-busy", provider.Chat, upstream)
			selected, err := provider.Find("legacy-busy")
			if err != nil {
				t.Fatal(err)
			}
			selected.Patience = provider.PatienceOff
			if err := provider.Save(*selected); err != nil {
				t.Fatal(err)
			}
			status := requestPolicyFront(t, `{"model":"legacy-busy/m","messages":[{"role":"user","content":"hi"}]}`).Code
			if status != 200 || upstream.n != 2 {
				t.Fatalf("legacy busy status lost its safe repeat: body=%s status=%d sends=%d", body, status, upstream.n)
			}
		})
	}
}
