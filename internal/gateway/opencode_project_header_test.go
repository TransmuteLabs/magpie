package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestOpenCodeSDKProjectHeader(t *testing.T) {
	const project, alternate = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const projectHeader = "X-Opencode-Project"
	const chatPath, messagesPath, responsesPath = "/v1/chat/completions", "/v1/messages", "/v1/responses"
	for _, module := range []string{"opencode", "fixture.opencode-lookalike"} {
		t.Run(module, func(t *testing.T) {
			manifest := extensionGatewayManifest()
			manifest.ID = module
			manifest.Contract.Version.Minor = 3
			goProvider := manifest.Providers[0]
			goProvider.ID, goProvider.Name = "opencode-go", "Fixture OpenCode Go"
			zenProvider := goProvider
			zenProvider.ID, zenProvider.Name = "opencode-zen", "Fixture OpenCode Zen"
			freeProvider := goProvider
			freeProvider.ID, freeProvider.Name = "opencode-zen-free", "Fixture OpenCode Zen Public"
			manifest.Providers[0] = goProvider
			manifest.Providers = append(manifest.Providers, zenProvider, freeProvider)
			installCanonicalExtensionManifest(t, manifest)
			_, secrets := newCaller(t, "OpenCode header fixture")
			server := New()
			t.Cleanup(server.idle.stop)

			scenarios := []struct {
				name, descriptor, path string
				values, want           []string
				passHeaders            bool
			}{
				{name: "go_chat", descriptor: goProvider.ID, path: chatPath, values: []string{project, alternate}, want: []string{project}},
				{name: "go_messages", descriptor: goProvider.ID, path: messagesPath, values: []string{project}, want: []string{project}},
				{name: "go_responses", descriptor: goProvider.ID, path: responsesPath, values: []string{project}, want: []string{project}},
				{name: "zen_chat", descriptor: zenProvider.ID, path: chatPath, values: []string{project}, want: []string{project}},
				{name: "zen_messages", descriptor: zenProvider.ID, path: messagesPath, values: []string{project}, want: []string{project}},
				{name: "zen_responses", descriptor: zenProvider.ID, path: responsesPath, values: []string{project}, want: []string{project}},
				{name: "absent", descriptor: goProvider.ID, path: chatPath},
				{name: "empty", descriptor: zenProvider.ID, path: chatPath, values: []string{""}},
				{name: "zen_free_chat", descriptor: freeProvider.ID, path: chatPath, values: []string{project}, want: []string{project}},
				{name: "configured_pass_headers", descriptor: goProvider.ID, path: chatPath,
					values: []string{project, alternate}, want: []string{project, alternate}, passHeaders: true},
			}
			for _, scenario := range scenarios {
				t.Run(scenario.name, func(t *testing.T) {
					id := provider.ExtensionProviderID(module, scenario.descriptor)
					if scenario.passHeaders {
						config := provider.Provider{ID: id, PassHeaders: []string{"x-opencode-project", "x-fixture-listed"}}
						if err := provider.Save(config); err != nil {
							t.Fatal(err)
						}
					}
					body := `{"model":"` + id + `/m1","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
					if scenario.path == responsesPath {
						body = `{"model":"` + id + `/m1","input":"hello"}`
					}
					request := httptest.NewRequest(http.MethodPost, scenario.path, strings.NewReader(body))
					request.RemoteAddr = "127.0.0.1:12345"
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Authorization", "Bearer "+secrets[0])
					request.Header.Set("Cookie", "fixture=private")
					request.Header.Set("X-Magpie-Caller-ID", "untrusted-caller")
					request.Header.Set("X-Opencode-Directory", "untrusted-directory")
					request.Header.Set("X-Opencode-Client", "untrusted-client")
					request.Header.Set("X-Fixture-Listed", "configured")
					request.Header.Set("X-Fixture-Unlisted", "unlisted")
					for _, value := range scenario.values {
						request.Header.Add(projectHeader, value)
					}
					reply := httptest.NewRecorder()
					server.Front().ServeHTTP(reply, request)
					execution := readCanonicalExecution(t, reply)
					if execution.Scope.Plugin != module || execution.Scope.Provider != scenario.descriptor ||
						execution.Request.Protocol != "chat" || execution.Request.Model != "m1" {
						t.Fatalf("unexpected SDK route: %s/%s, protocol %s, model %s",
							execution.Scope.Plugin, execution.Scope.Provider, execution.Request.Protocol, execution.Request.Model)
					}
					var upstream struct {
						Stream bool
					}
					if err := json.Unmarshal(execution.Request.Body, &upstream); err != nil || upstream.Stream != (scenario.path != chatPath) {
						t.Fatalf("SDK stream = %v for %s (%v)", upstream.Stream, scenario.path, err)
					}
					want := scenario.want
					if module != "opencode" && !scenario.passHeaders {
						want = nil
					}
					if got := execution.Request.Headers.Values(projectHeader); !slices.Equal(got, want) {
						t.Errorf("SDK project header = %q, want %q", got, want)
					}
					for _, name := range []string{"Authorization", "Cookie", "X-Magpie-Caller-ID",
						"X-Opencode-Directory", "X-Opencode-Client", "X-Fixture-Unlisted"} {
						if values := execution.Request.Headers.Values(name); len(values) != 0 {
							t.Errorf("SDK received unlisted inbound header %s = %q", name, values)
						}
					}
					var listed []string
					if scenario.passHeaders {
						listed = []string{"configured"}
					}
					if got := execution.Request.Headers.Values("X-Fixture-Listed"); !slices.Equal(got, listed) {
						t.Errorf("configured SDK header = %q, want %q", got, listed)
					}
				})
			}
		})
	}
}
