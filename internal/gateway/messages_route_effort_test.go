package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/provider"
)

func TestMessagesRouteOverridesTurnEffort(t *testing.T) {
	fresh(t)
	upstream := &adaptiveVendor{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	const member = "relay/claude-opus-5-5"
	if err := provider.Save(provider.Provider{
		ID: "relay", Name: "Relay", Key: "fixture", Anthropic: server.URL, Models: []string{"claude-opus-5-5"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetModelEfforts(member, []string{"low", "medium", "high", "max"}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []provider.Group{
		{Name: "Fixed", Members: []string{member + ":low"}, Routing: provider.Ordered},
		{Name: "Ruled", Members: []string{member}, Routing: provider.Ordered,
			Rules: []provider.Rule{{Use: member, UseEffort: "low", Tokens: 1}}},
		{Name: "Mapped", Members: []string{member}, Routing: provider.Ordered,
			EffortMaps: map[string]map[string]string{member: {"max": "low"}}},
	} {
		if err := provider.SaveGroup(group); err != nil {
			t.Fatal(err)
		}
	}
	handler := New().Handler()
	for _, test := range []struct {
		name, model, asked, top, turn string
	}{
		{"direct", member, "max", "max", "high"},
		{"fixed", "group/fixed", "max", "low", "low"},
		{"fixed with matching top effort", "group/fixed", "low", "low", "low"},
		{"rule", "group/ruled", "max", "low", "low"},
		{"map", "group/mapped", "max", "low", "low"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"model":"` + test.model + `","max_tokens":32000,"thinking":{"type":"adaptive","display":"summarized"},
				"output_config":{"effort":"` + test.asked + `"},"messages":[
					{"role":"system","content":[],"output_config":{"effort":"medium"}},
					{"role":"user","content":"previous question"},
					{"role":"assistant","content":"previous answer"},
					{"role":"system","content":[{"type":"text","text":"effort changed <notice>"}],"output_config":{"effort":"high"}},
					{"role":"user","content":"what is two plus two?"}]}`
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body)
			}
			sent, err := json.Marshal(upstream.last())
			if err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{
				"output_config.effort":            test.top,
				"messages.0.output_config.effort": "medium",
				"messages.3.output_config.effort": test.turn,
				"messages.3.content.0.text":       "effort changed <notice>",
				"messages.4.content":              "what is two plus two?",
			} {
				if got := gjson.GetBytes(sent, path).String(); got != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
		})
	}
}

func TestFixedMessagesEffortLeavesInsufficientBudget(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"user","content":"hi"}]}`)
	if got := withFixedEffort(provider.Anthropic, body, "low"); string(got) != string(body) {
		t.Fatalf("short budget changed: %s", got)
	}
}
