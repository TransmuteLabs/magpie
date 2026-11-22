package provider

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/settings"
)

// Native is the API a client does best to ask a model on, for the gateway
// to relay it as it is.
func TestNative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	// Copilot's model list says which APIs serve each model
	if err := catalog.SaveLive("copilot", "https://api.githubcopilot.com", []catalog.Model{
		{ID: "gpt-5.5", APIs: []string{"responses"}}, {ID: "claude-sonnet-5", APIs: []string{"chat", "anthropic"}}}); err != nil {
		t.Fatal(err)
	}
	openai := Provider{ID: "openai", Chat: "https://api.openai.com/v1", Responses: "https://api.openai.com/v1"}
	copilot := Provider{ID: "copilot", Chat: "https://api.githubcopilot.com", Responses: "https://api.githubcopilot.com", Anthropic: "https://api.githubcopilot.com"}
	for _, c := range []struct {
		name  string
		p     Provider
		model string
		want  Protocol
	}{
		{"chat only", Provider{ID: "relay", Chat: "https://relay.test/v1"}, "glm-4.6", Chat},
		{"responses only", Provider{ID: "resp", Responses: "https://resp.test/v1"}, "m", Responses},
		{"ChatGPT sign-in", Provider{ID: "codex", Responses: CodexBase, Account: &Account{Agent: "codex"}}, "gpt-5.5", Responses},
		{"GPT on OpenAI's API", openai, "gpt-5.5", Responses},
		{"o-series on OpenAI's API", openai, "o3", Responses},
		{"other on OpenAI's API", openai, "whisper-x", Chat},
		{"Chat and Responses elsewhere", Provider{ID: "both", Chat: "https://x.test/v1", Responses: "https://x.test/v1"}, "gpt-5.5", Chat},
		// the API the user gave a custom provider's Base URL as, where a
		// Chat URL was found beside it (nb5p on Discord)
		{"Responses picked beside Chat", Provider{ID: "mygateway", Chat: "https://x.test/v1", Responses: "https://x.test/v1", BaseAPI: "responses"}, "gpt-6.1-sol", Responses},
		{"Chat picked beside Responses", Provider{ID: "mygateway", Chat: "https://x.test/v1", Responses: "https://x.test/v1", BaseAPI: "chat"}, "gpt-6.1-sol", Chat},
		{"Responses picked, Claude on Messages", Provider{ID: "mygateway", Responses: "https://x.test/v1", Anthropic: "https://x.test", BaseAPI: "responses"}, "claude-sonnet-5", Anthropic},
		{"Responses picked, no Responses URL", Provider{ID: "mygateway", Chat: "https://x.test/v1", BaseAPI: "responses"}, "m", Chat},
		{"anthropic only", Provider{ID: "anthropic", Anthropic: "https://api.anthropic.com"}, "claude-sonnet-5", Anthropic},
		{"Copilot GPT", copilot, "gpt-5.5", Responses},
		{"Copilot Claude", copilot, "claude-sonnet-5", Anthropic},
		{"Claude Code", Provider{ID: "claude", Anthropic: "https://api.anthropic.com", Account: &Account{Agent: "claude"}}, "claude-sonnet-5", ""},
		{"nothing", Provider{ID: "none"}, "m", ""},
	} {
		if got := c.p.Native(c.model); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAgentModelProtocols(t *testing.T) {
	providerFileHome(t)
	prefs := settings.Load()
	prefs.ModelAPIs = map[string]string{
		"hybrid/gemini-only":  "gemini",
		"preferred/chat-only": "chat",
	}
	if err := settings.Save(prefs); err != nil {
		t.Fatal(err)
	}
	hybrid := Provider{ID: "hybrid", Chat: "http://127.0.0.1:1/v1", Gemini: "http://127.0.0.1:1/v1beta"}
	preferred := Provider{ID: "preferred", Chat: "http://127.0.0.1:1/v1", Responses: "http://127.0.0.1:1/v1", BaseAPI: "responses"}
	translated := Provider{ID: "code-assist", Account: &Account{codeAssist: "http://127.0.0.1:1"}}
	for _, test := range []struct {
		name      string
		provider  Provider
		model     string
		api       Protocol
		endpoints []string
	}{
		{"native before Gemini", hybrid, "both", Chat, []string{"/v1/chat/completions"}},
		{"explicit Gemini", hybrid, "gemini-only", Gemini, nil},
		{"translated Code Assist", translated, "model", Gemini, nil},
		{"preferred base", preferred, "both", Responses, []string{"/v1/chat/completions", "/v1/responses"}},
		{"explicit Chat", preferred, "chat-only", Chat, []string{"/v1/chat/completions"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, endpoints := AgentModelProtocols(Entry{Provider: &test.provider, Model: test.model}, nil)
			if api != test.api || !slices.Equal(endpoints, test.endpoints) {
				t.Fatalf("paired projection %q %v, want %q %v", api, endpoints, test.api, test.endpoints)
			}
		})
	}
}
