package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/library"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/testenv"
)

func TestLibraryAccountTargetAndBindGuards(t *testing.T) {
	home := sandboxHome(t)
	command := filepath.Join(home, "program")
	script := filepath.Join(home, "script.js")
	if err := os.WriteFile(command, []byte("program"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("script"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_LIBRARY_TEST_COMMAND", command)
	t.Setenv("MAGPIE_LIBRARY_TEST_SECRET", "hidden-env-value")
	for _, s := range []library.Server{
		{Name: "remote", Transport: "http", URL: "https://example.test/mcp", Headers: map[string]string{"Authorization": "hidden-header"}},
		{Name: "command", Transport: "stdio", Command: "${MAGPIE_LIBRARY_TEST_COMMAND}",
			Args: []string{script, "--token=${MAGPIE_LIBRARY_TEST_SECRET}", "$MAGPIE_LIBRARY_TEST_SECRET"},
			Env:  map[string]string{"TOKEN": "${MAGPIE_LIBRARY_TEST_SECRET}"}},
	} {
		if _, err := library.SaveServer("", s); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	libraryRoutes(mux, folderOnly{})
	get := func(name string) (library.PluginAccountTarget, string) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/library/servers/account-target?name="+name, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("target preview status %d: %s", w.Code, w.Body.String())
		}
		var target library.PluginAccountTarget
		if err := json.Unmarshal(w.Body.Bytes(), &target); err != nil {
			t.Fatal(err)
		}
		return target, w.Body.String()
	}
	remote, raw := get("remote")
	if remote.URL != "https://example.test/mcp" || remote.Fingerprint == "" || strings.Contains(raw, "hidden-header") {
		t.Fatalf("unsafe or incorrect HTTP preview: %s", raw)
	}
	local, raw := get("command")
	if local.Command != "${MAGPIE_LIBRARY_TEST_COMMAND}" || len(local.Args) != 3 || local.Args[0] != script ||
		local.Args[1] != "--token=${MAGPIE_LIBRARY_TEST_SECRET}" || local.Args[2] != "$MAGPIE_LIBRARY_TEST_SECRET" ||
		len(local.Environment) != 1 || local.Environment[0] != "TOKEN" || local.Fingerprint == "" ||
		strings.Contains(raw, "hidden-env-value") || strings.Contains(raw, command) {
		t.Fatalf("unsafe or incorrect command preview: %s", raw)
	}
	t.Setenv("MAGPIE_LIBRARY_TEST_SECRET", "rotated-env-value")
	rotated, raw := get("command")
	if rotated.Fingerprint == local.Fingerprint || rotated.Command != local.Command ||
		len(rotated.Args) != 3 || rotated.Args[1] != local.Args[1] || rotated.Args[2] != local.Args[2] ||
		strings.Contains(raw, "rotated-env-value") || strings.Contains(raw, command) {
		t.Fatalf("expanded command did not affect the private fingerprint safely: %s", raw)
	}
	local = rotated
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/library/servers/"+path, bytes.NewReader(b)))
		return w
	}
	for _, body := range []map[string]any{
		{"name": "remote", "provider": "zcode", "account": "zcode#slot", "scope": "scope", "generation": "gen"},
		{"name": "remote", "provider": "zcode", "account": "zcode#slot", "scope": "scope", "targetFingerprint": remote.Fingerprint},
		{"name": "remote", "provider": "zcode", "account": "zcode#slot", "generation": "gen", "targetFingerprint": remote.Fingerprint},
	} {
		if w := post("account-bind", body); w.Code != http.StatusBadRequest {
			t.Fatalf("incomplete context accepted: %d %s", w.Code, w.Body.String())
		}
	}
	for _, body := range []map[string]any{
		{"name": "remote"},
		{"binding": map[string]any{"provider": "zcode", "account": "zcode#slot", "scope": "scope"}},
		{"name": "remote", "binding": map[string]any{"account": "zcode#slot", "scope": "scope"}},
		{"name": "remote", "binding": map[string]any{"provider": "zcode", "scope": "scope"}},
		{"name": "remote", "binding": map[string]any{"provider": "zcode", "account": "zcode#slot"}},
	} {
		if w := post("account-unbind/scoped", body); w.Code != http.StatusBadRequest {
			t.Fatalf("scoped unbind without a complete displayed binding accepted: %d %s", w.Code, w.Body.String())
		}
	}
	for _, body := range []map[string]any{
		{"name": "remote", "provider": "zcode", "account": "other#slot", "scope": "scope", "generation": "gen", "targetFingerprint": remote.Fingerprint},
		{"name": "remote", "provider": "other", "account": "other#slot", "scope": "scope", "generation": "gen", "targetFingerprint": remote.Fingerprint},
		{"name": "command", "provider": "zcode", "account": "zcode#slot", "scope": "scope", "generation": "gen", "targetFingerprint": local.Fingerprint},
	} {
		if w := post("account-bind", body); w.Code >= 200 && w.Code < 300 {
			t.Fatalf("unsafe account bind accepted: %d %s", w.Code, w.Body.String())
		}
	}
	for _, name := range []string{"remote", "command"} {
		binding, err := library.LibraryPluginAccountBinding(name)
		if err != nil || binding != nil {
			t.Fatalf("unexpected binding for %s: %+v %v", name, binding, err)
		}
	}
}

func TestLibraryAccountUnbindScopedAndLegacyCurrent(t *testing.T) {
	sandboxHome(t)
	file := filepath.Join(filepath.Dir(library.Dir()), "library.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	server := library.Server{Name: "legacy", Transport: "http", URL: "https://example.test/mcp"}
	seed := func(binding library.PluginAccount) {
		t.Helper()
		server.PluginAccount = &binding
		b, err := json.Marshal(library.Library{MCP: []*library.Server{&server}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := library.PluginAccount{Provider: "legacy-provider", Account: "legacy-provider#old", Scope: "old-scope"}
	replacement := library.PluginAccount{Provider: "legacy-provider", Account: "legacy-provider#new", Scope: "new-scope"}
	seed(old)
	mux := http.NewServeMux()
	libraryRoutes(mux, folderOnly{})
	preview := httptest.NewRecorder()
	mux.ServeHTTP(preview, httptest.NewRequest("GET", "/api/library/servers/account-target?name=legacy&binding=1", nil))
	if preview.Code != http.StatusOK {
		t.Fatalf("binding preview: %d %s", preview.Code, preview.Body.String())
	}
	var displayed struct {
		Binding *library.PluginAccount `json:"binding"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &displayed); err != nil {
		t.Fatal(err)
	}
	if displayed.Binding == nil || *displayed.Binding != old {
		t.Fatalf("wrong displayed binding: %+v", displayed.Binding)
	}
	seed(replacement)
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/library/servers/"+path, bytes.NewReader(b)))
		return w
	}
	if w := post("account-unbind/scoped", map[string]any{"name": "legacy", "binding": displayed.Binding}); w.Code >= 200 && w.Code < 300 {
		t.Fatalf("stale scoped unbind accepted: %d %s", w.Code, w.Body.String())
	}
	if binding, err := library.LibraryPluginAccountBinding("legacy"); err != nil || binding == nil || *binding != replacement {
		t.Fatalf("stale scoped unbind removed the replacement: %+v %v", binding, err)
	}
	if w := post("account-unbind", map[string]any{"name": "legacy"}); w.Code != http.StatusOK {
		t.Fatalf("legacy current-at-call unbind failed: %d %s", w.Code, w.Body.String())
	}
	if binding, err := library.LibraryPluginAccountBinding("legacy"); err != nil || binding != nil {
		t.Fatalf("legacy unbind did not remove the current binding: %+v %v", binding, err)
	}
}

func TestLibraryStdioAccountBindingHTTPRoundTrip(t *testing.T) {
	bun := testenv.Bun(t)
	home := sandboxHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Cleanup(plugin.Settle)
	fixture := filepath.Join(home, "native-fixture.mjs")
	source := `export const Fixture = async () => ({
  config(c) {
    c.provider ??= {};
    c.provider.zcode = {name: "Fixture", npm: "@ai-sdk/openai-compatible",
      api: "https://fixture.invalid/v1",
      models: {one: {name: "One", limit: {context: 1000, output: 100}}}};
  },
  auth: {
    provider: "zcode", methods: [{type: "api", label: "Fixture"}],
    accountOperations: ["context", "mcp.headers"],
    accountPreflight({operation, params}) {
      if (operation === "mcp.headers" && (params?.stdio !== true || params?.url))
        throw new Error("untrusted");
    },
    async account(getAuth, _, {operation, expectedScope, expectedGeneration}) {
      const a = await getAuth();
      if (!a || expectedScope && expectedScope !== a.accountScope ||
          expectedGeneration && expectedGeneration !== a.accountEpoch)
        throw Object.assign(new Error("changed"), {kind: "account_changed"});
      const context = {scope: a.accountScope, generation: a.accountEpoch,
        origin: "https://fixture.invalid", family: "fixture", userId: "user",
        kind: "individual", organization: "", project: "", product: ""};
      return {context, data: operation === "mcp.headers" ?
        {headers: {Authorization: "Bearer " + a.key}} : {}};
    }
  }
})`
	if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := plugin.Add(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	account, err := plugin.Import(ctx, "zcode", map[string]any{
		"type": "api", "key": "fixture-secret", "accountId": "user",
		"accountScope": "scope", "accountEpoch": "generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "program")
	if err := os.WriteFile(command, []byte("trusted fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := library.SaveServer("", library.Server{Name: "command", Transport: "stdio", Command: command}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	libraryRoutes(mux, folderOnly{})
	zcodeAccountRoutes(mux)
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(data)).WithContext(ctx))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "fixture-secret") {
			t.Fatalf("%s leaked the account credential: %s", path, w.Body.String())
		}
		return w
	}
	var choices struct {
		Available bool `json:"available"`
		Accounts  []struct {
			Provider       string `json:"provider"`
			PluginProvider string `json:"pluginProvider"`
			Account        string `json:"account"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(request("GET", "/api/usage/zcode-accounts", nil).Body.Bytes(), &choices); err != nil {
		t.Fatal(err)
	}
	var selected struct{ Provider, PluginProvider, Account string }
	for _, c := range choices.Accounts {
		if c.PluginProvider == "zcode" && c.Account == account {
			selected = struct{ Provider, PluginProvider, Account string }{c.Provider, c.PluginProvider, c.Account}
		}
	}
	if !choices.Available || selected.Provider == "" {
		t.Fatalf("fixture account missing from passive choices: %+v", choices)
	}
	var captured plugin.AccountResult
	if err := json.Unmarshal(request("POST", "/api/usage/zcode-account", map[string]any{
		"provider": selected.Provider, "account": selected.Account, "operation": "context",
	}).Body.Bytes(), &captured); err != nil {
		t.Fatal(err)
	}
	var target library.PluginAccountTarget
	if err := json.Unmarshal(request("GET", "/api/library/servers/account-target?name=command", nil).Body.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	request("POST", "/api/library/servers/account-bind", map[string]any{
		"name": "command", "provider": selected.PluginProvider, "account": selected.Account,
		"scope": captured.Context.Scope, "generation": captured.Context.Generation,
		"targetFingerprint": target.Fingerprint, "trustCommand": true,
	})
	var view struct {
		Servers []library.Server `json:"servers"`
	}
	if err := json.Unmarshal(request("GET", "/api/library", nil).Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var displayed *library.PluginAccount
	for _, server := range view.Servers {
		if server.Name == "command" {
			displayed = server.PluginAccount
		}
	}
	if displayed == nil || displayed.CommandDigest == "" || displayed.Account != account {
		t.Fatalf("GUI view lost command trust or account identity: %+v", displayed)
	}
	var bindingPreview struct {
		Binding *library.PluginAccount `json:"binding"`
	}
	if err := json.Unmarshal(request("GET", "/api/library/servers/account-target?name=command&binding=1", nil).Body.Bytes(), &bindingPreview); err != nil {
		t.Fatal(err)
	}
	if bindingPreview.Binding == nil || *bindingPreview.Binding != *displayed {
		t.Fatalf("binding preview differs from the displayed identity: %+v %+v", bindingPreview.Binding, displayed)
	}
	stale := *displayed
	stale.CommandDigest += "-stale"
	body, err := json.Marshal(map[string]any{"name": "command", "binding": stale})
	if err != nil {
		t.Fatal(err)
	}
	rejected := httptest.NewRecorder()
	mux.ServeHTTP(rejected, httptest.NewRequest("POST", "/api/library/servers/account-unbind/scoped", bytes.NewReader(body)).WithContext(ctx))
	if rejected.Code >= 200 && rejected.Code < 300 {
		t.Fatalf("scoped unbind accepted a stale command digest: %d %s", rejected.Code, rejected.Body.String())
	}
	if binding, err := library.LibraryPluginAccountBinding("command"); err != nil || binding == nil || *binding != *displayed {
		t.Fatalf("stale command digest removed the binding: %+v %v", binding, err)
	}
	request("POST", "/api/library/servers/account-unbind/scoped", map[string]any{"name": "command", "binding": displayed})
	if binding, err := library.LibraryPluginAccountBinding("command"); err != nil || binding != nil {
		t.Fatalf("GUI unbind did not remove the displayed binding: %+v %v", binding, err)
	}
}
