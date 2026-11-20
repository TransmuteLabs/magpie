package agent

import (
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/edit"
)

func TestClaudeDisconnectKeepsCredentialsWithTheirEndpoint(t *testing.T) {
	const (
		routeURL      = "env.ANTHROPIC_BASE_URL"
		routeToken    = "env.ANTHROPIC_AUTH_TOKEN"
		originalURL   = "https://original.example"
		replacement   = "https://replacement.example"
		originalToken = "original-token"
		userToken     = "user-edited-token"
	)
	for _, scenario := range []struct {
		name          string
		original      string
		editedURL     string
		editToken     string
		savedCaller   bool
		expectedToken string
	}{
		{name: "restore original", original: originalToken, expectedToken: originalToken},
		{name: "originally absent"},
		{name: "changed endpoint", original: originalToken, editedURL: replacement},
		{name: "saved caller key", savedCaller: true},
		{name: "user token", original: originalToken, editedURL: replacement, editToken: userToken, expectedToken: userToken},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			home := syncHome(t)
			caller, err := access.Update("add-key", access.Change{Name: "claude-disconnect"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, ".claude", "settings.json")
			writeFile(t, path, `{"model":"opus","env":{"ANTHROPIC_BASE_URL":"`+originalURL+`"}}`)
			initialToken := scenario.original
			if scenario.savedCaller {
				initialToken = caller
			}
			if initialToken != "" {
				if err := edit.SetJSON(path, edit.KV{Path: routeToken, Value: initialToken}); err != nil {
					t.Fatal(err)
				}
			}
			agent := claude(home)
			if err := agent.Connect(); err != nil {
				t.Fatal(err)
			}
			if !agent.Wired() {
				t.Fatal("Connect did not route Claude Code")
			}
			nextToken := caller
			if scenario.editToken != "" {
				nextToken = scenario.editToken
			}
			changes := []edit.KV{{Path: routeToken, Value: nextToken}}
			expectedURL := originalURL
			if scenario.editedURL != "" {
				expectedURL = scenario.editedURL
				changes = append(changes, edit.KV{Path: routeURL, Value: expectedURL})
			}
			if err := edit.SetJSON(path, changes...); err != nil {
				t.Fatal(err)
			}
			if err := agent.Disconnect(); err != nil {
				t.Fatal(err)
			}
			if got, exists := edit.GetJSON(path, routeURL); !exists || got != expectedURL {
				t.Errorf("Disconnect leaves endpoint %q; want %q", got, expectedURL)
			}
			got, exists := edit.GetJSON(path, routeToken)
			if got == caller || got != scenario.expectedToken || exists != (scenario.expectedToken != "") {
				t.Errorf("Disconnect selected the wrong credential (present=%v, caller=%v)", exists, got == caller)
			}
		})
	}
}
