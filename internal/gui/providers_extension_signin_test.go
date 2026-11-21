package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func readProviderSignInState(t *testing.T, handler http.Handler) providersJSON {
	t.Helper()
	response := do(handler, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	var state providersJSON
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil {
		t.Fatalf("/api/providers returned %d", response.Code)
	}
	return state
}

func TestProvidersOfferSDKSubscriptionBeforeFirstAccount(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := extension.Default()
	manifest := authfixture.Manifest()
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	source := t.TempDir()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		sdk.ManifestFile: body,
		manifest.Entry:   []byte("discovery must not execute this file"),
		"package.json":   []byte(`{"name":"auth-fixture","magpie":{"page":{"title":"Fixture","file":"index.html"}}}`),
		"index.html":     []byte(`<!doctype html>`),
	} {
		if err := os.WriteFile(filepath.Join(source, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installed, err := extension.NewCatalog(manager.Namespace()).Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	state := readProviderSignInState(t, mux)
	want := provider.ExtensionProviderID(installed.Manifest.ID, "vendor")
	if len(state.SDKSubscriptions) != 1 || state.SDKSubscriptions[0].ID != want ||
		state.SDKSubscriptions[0].Name != "Vendor" || state.SDKSubscriptions[0].Page == "" {
		t.Fatalf("SDK subscription with an existing sign-in page is missing before account creation: %+v", state.SDKSubscriptions)
	}
	for _, item := range state.Providers {
		if item.ID == want {
			t.Fatal("a sign-in choice without an account must not become a routed provider")
		}
	}
	if manager.Running(manifest.ID) {
		t.Fatal("listing sign-in choices started the SDK worker")
	}
}

func TestProviderSignInUsesOrdinaryAccountsModelsAndUsage(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	prefs := settings.Load()
	prefs.SignInLinkOnly = true
	if err := settings.Save(prefs); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	manifest := authfixture.Manifest()
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ProviderSignIn, sdk.OutboundExecutor, sdk.UsageReporter)
	manifest.Providers[0].Models = []sdk.Model{{ID: "fixture-model", Name: "Fixture Model"}}
	manifest.AuthBindings = append(manifest.AuthBindings, sdk.WindowAuthBindingDescriptor{
		ID: "provider.signin", From: sdk.ProviderSignInPart, Provider: "vendor", Reauthorize: true,
		Methods: []sdk.WindowAuthMethod{{ID: "device", Mode: "poll", Label: "Device code"}, {
			ID: "key", Mode: "poll", Label: "API key", Fields: []sdk.AuthInputField{
				{Key: "key", Label: "API key", Type: "secret"},
				{Key: "site", Label: "Site", Type: "select", Optional: true, Choices: []sdk.AuthInputChoice{{Value: "main", Label: "Main"}}},
			},
		}},
	})
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	source := t.TempDir()
	build := proc.CommandContext(testenv.DeadlineContext(t, 5*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(source, manifest.Entry), "../extension/testdata/authworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build auth fixture: %v %s", err, output)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		sdk.ManifestFile: string(raw),
		"package.json":   `{"name":"auth-fixture","magpie":{"page":{"title":"Fixture","file":"index.html"}}}`,
		"index.html":     `<!doctype html>`,
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	extensionWindowAuthRoutes(mux)
	id := provider.ExtensionProviderID(manifest.ID, "vendor")
	selected := func() extensionSubscriptionJSON {
		t.Helper()
		state := readProviderSignInState(t, mux)
		if len(state.SDKSubscriptions) != 1 {
			t.Fatalf("subscription choices: %+v", state.SDKSubscriptions)
		}
		row := state.SDKSubscriptions[0]
		if row.ID != id || row.Revision == "" || row.Page != "" || len(row.Methods) != 2 ||
			row.Methods[1].Fields[0].Type != "secret" || !row.Reauthorize {
			t.Fatalf("unexpected ordinary SDK sign-in: %+v", row)
		}
		return row
	}
	post := func(path, body string, accepted bool) *httptest.ResponseRecorder {
		t.Helper()
		response := do(mux, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if (response.Code == http.StatusOK) != accepted {
			t.Fatalf("%s (%t): %d %s", path, accepted, response.Code, response.Body.String())
		}
		for _, token := range []string{"PRIVATE", "fixture-secret", "continuation"} {
			if bytes.Contains(response.Body.Bytes(), []byte(token)) {
				t.Fatal("sign-in returned private data")
			}
		}
		return response
	}
	signIn := func(revision, parent, operation, document string, accepted bool) *httptest.ResponseRecorder {
		t.Helper()
		return post("/api/provider/sdk-signin", `{"id":"`+id+`","revision":"`+revision+
			`","parent":"`+parent+`","operation":"`+operation+`",`+document+`}`, accepted)
	}
	newFlow := func(row extensionSubscriptionJSON, parent, options string) sdk.WindowAuthFlowView {
		t.Helper()
		response := signIn(row.Revision, parent, "start", `"request":"start","options":`+options, true)
		var view struct {
			sdk.WindowAuthFlowView
			LinkOnly bool `json:"linkOnly"`
		}
		if json.Unmarshal(response.Body.Bytes(), &view) != nil || view.State != "waiting" || !view.LinkOnly ||
			view.UserCode != "SAFE-CODE" || view.URL == "" || view.RetryAfterMs < 1000 ||
			response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid public sign-in flow: %+v", view)
		}
		return view.WindowAuthFlowView
	}
	finish := func(row extensionSubscriptionJSON, parent string, view sdk.WindowAuthFlowView, generation string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(time.Duration(view.RetryAfterMs) * time.Millisecond)
			response := signIn(row.Revision, parent, "finish", `"flow":"`+view.Flow+`"`, true)
			if json.Unmarshal(response.Body.Bytes(), &view) != nil {
				t.Fatal("invalid finish response")
			}
			if view.State == "complete" {
				if view.Account == nil || view.Account.ID != "chosen" || view.Account.Generation != generation {
					t.Fatalf("wrong account receipt: %+v", view)
				}
				return
			}
			if view.State != "waiting" {
				t.Fatalf("unexpected finish state: %+v", view)
			}
		}
		t.Fatal("sign-in did not complete")
	}
	first := selected()
	for _, malformed := range []string{
		`{"method":"key","input":{}}`,
		`{"method":"key","input":{"key":"one","site":"other"}}`,
		`{"method":"key","input":{"key":"one","key":"two"}}`,
		`{"method":"device","input":{"key":"unexpected"}}`,
	} {
		signIn(first.Revision, "invalid", "start", `"request":"start","options":`+malformed, false)
	}
	if manager.Running(manifest.ID) {
		t.Fatal("listing or rejecting a form started the worker")
	}
	validOptions := `{"method":"key","input":{"key":"fixture-secret","site":"main"}}`
	canceled := newFlow(first, "cancel", validOptions)
	signIn(first.Revision, "foreign", "finish", `"flow":"`+canceled.Flow+`"`, false)
	pages, _, err := extensionWindowParts(t.Context())
	if err != nil || len(pages) != 1 {
		t.Fatalf("fixture window missing: %v", err)
	}
	windowAuth := func(binding, operation, document string, accepted bool) *httptest.ResponseRecorder {
		t.Helper()
		return post("/api/plugin-pages/auth", `{"ref":"`+pages[0].Ref+`","binding":"`+binding+
			`","parent":"cancel","operation":"`+operation+`",`+document+`}`, accepted)
	}
	windowAuth("provider.signin", "finish", `"flow":"`+canceled.Flow+`"`, false)
	windowAuth("signin", "finish", `"flow":"`+canceled.Flow+`"`, false)
	window := windowAuth("signin", "start", `"request":"start","options":{"method":"device"}`, true)
	var windowView sdk.WindowAuthFlowView
	if json.Unmarshal(window.Body.Bytes(), &windowView) != nil || windowView.State != "waiting" {
		t.Fatal("window grant did not start a separate flow")
	}
	signIn(first.Revision, "cancel", "finish", `"flow":"`+windowView.Flow+`"`, false)
	windowAuth("signin", "cancel", `"flow":"`+windowView.Flow+`"`, true)
	var canceledView sdk.WindowAuthFlowView
	response := signIn(first.Revision, "cancel", "cancel", `"flow":"`+canceled.Flow+`"`, true)
	if json.Unmarshal(response.Body.Bytes(), &canceledView) != nil || canceledView.State != "canceled" {
		t.Fatal("cancel did not reach the broker")
	}
	revoked := newFlow(first, "before-disable", validOptions)
	for _, on := range []bool{false, true} {
		if err := manager.SetEnabled(t.Context(), manifest.ID, on); err != nil {
			t.Fatal(err)
		}
	}
	current := selected()
	if current.Revision == first.Revision {
		t.Fatal("enablement reused an obsolete Providers selection")
	}
	signIn(first.Revision, "before-disable", "finish", `"flow":"`+revoked.Flow+`"`, false)
	signIn(current.Revision, "before-disable", "finish", `"flow":"`+revoked.Flow+`"`, false)
	revoked = newFlow(current, "before-config", validOptions)
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"fail":false}`)); err != nil {
		t.Fatal(err)
	}
	afterConfig := selected()
	if afterConfig.Revision == current.Revision {
		t.Fatal("configuration change reused the Providers selection")
	}
	signIn(current.Revision, "before-config", "finish", `"flow":"`+revoked.Flow+`"`, false)
	signIn(afterConfig.Revision, "before-config", "finish", `"flow":"`+revoked.Flow+`"`, false)
	revoked = newFlow(afterConfig, "before-update", validOptions)
	manifest.Version = "1.0.1"
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	current = selected()
	if current.Revision == afterConfig.Revision {
		t.Fatal("package replacement reused the Providers selection")
	}
	signIn(afterConfig.Revision, "before-update", "finish", `"flow":"`+revoked.Flow+`"`, false)
	signIn(current.Revision, "before-update", "finish", `"flow":"`+revoked.Flow+`"`, false)
	view := newFlow(current, "first-account", validOptions)
	finish(current, "first-account", view, "1")
	state := readProviderSignInState(t, mux)
	found := false
	for _, row := range state.Providers {
		if row.ID == id {
			found = row.Account != nil && row.Account.Agent == id && len(row.Models) == 1 &&
				row.Models[0].ID == "fixture-model" && len(row.Account.Logins) == 1 &&
				row.AccountUsers["chosen"] == "reader@example.test" && row.Account.Logins[0].User == "chosen" && row.Account.Logins[0].ExtensionGeneration == "1"
		}
	}
	if !found {
		t.Fatal("completed sign-in did not publish the normal provider, model and account")
	}
	usage := do(mux, httptest.NewRequest(http.MethodGet, "/api/login/usage?agent="+id, nil))
	if usage.Code != http.StatusOK || !bytes.Contains(usage.Body.Bytes(), []byte(`"name":"Weekly"`)) ||
		!bytes.Contains(usage.Body.Bytes(), []byte(`"used":17`)) {
		t.Fatalf("account did not report ordinary Usage: %s", usage.Body.String())
	}
	reauthorize := newFlow(current, "reauthorize", `{"method":"device","account":{"id":"chosen","expectedGeneration":"1"}}`)
	finish(current, "reauthorize", reauthorize, "2")
	signIn(current.Revision, "stale-account", "start", `"request":"start","options":{"method":"device","account":{"id":"chosen","expectedGeneration":"1"}}`, false)

	other, err := manager.Store().SaveAccount(extension.Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "other", Auth: json.RawMessage(`{"token":"PRIVATE-OTHER"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "logout.jsonl")
	config, err := manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(authfixture.Config{Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, config.Version, raw); err != nil {
		t.Fatal(err)
	}
	logout := func(user, generation string, accepted bool) {
		t.Helper()
		post("/api/login/forget", `{"agent":"`+id+`","account":{"id":"`+user+`","expectedGeneration":"`+generation+`"}}`, accepted)
	}
	logout("chosen", "1", false)
	logout("chosen", "02", false)
	post("/api/login/forget", `{"agent":"`+id+`","user":"chosen","account":null}`, false)
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("a stale or invalid generation reached auth.logout: %v", err)
	}
	logout("chosen", "2", true)
	if _, err := manager.Store().GetAccount(manifest.ID, "vendor", "chosen"); err == nil {
		t.Fatal("sign out left the selected credentials in the active store")
	}
	retained, err := manager.Store().GetAccount(manifest.ID, "vendor", "other")
	if err != nil || retained.Generation != other.Generation || !bytes.Equal(retained.Auth, other.Auth) {
		t.Fatalf("sign out changed the other account's credentials or generation: %v", err)
	}
	state = readProviderSignInState(t, mux)
	if !slices.ContainsFunc(state.Providers, func(row providerJSON) bool {
		return row.ID == id && row.Account != nil && len(row.Account.Logins) == 1 &&
			row.Account.Logins[0].User == "other" && len(row.Models) == 1
	}) {
		t.Fatal("sign out changed the surviving account or its ordinary provider and model")
	}
	usage = do(mux, httptest.NewRequest(http.MethodGet, "/api/login/usage?agent="+id, nil))
	var quotas map[string]provider.SubscriptionQuota
	if usage.Code != http.StatusOK || json.Unmarshal(usage.Body.Bytes(), &quotas) != nil ||
		len(quotas) != 1 || len(quotas["other"].Windows) != 1 || quotas["other"].Windows[0].Used != 23 {
		t.Fatalf("sign out left stale usage or dropped the surviving account: %s", usage.Body.String())
	}
	logout("other", "1", true)
	if slices.ContainsFunc(readProviderSignInState(t, mux).Providers, func(row providerJSON) bool { return row.ID == id }) ||
		slices.ContainsFunc(provider.Served(), func(entry provider.Entry) bool { return entry.Provider.ID == id }) {
		t.Fatal("signing out the last account left an ordinary provider or served model")
	}
	selected()
	calls, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(calls), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("wanted two logout calls, got %d", len(lines))
	}
	for index, want := range []sdk.Scope{
		{Plugin: manifest.ID, Provider: "vendor", Account: "chosen", Generation: 2},
		{Plugin: manifest.ID, Provider: "vendor", Account: "other", Generation: 1},
	} {
		var call struct {
			Method string
			Scope  sdk.Scope
		}
		if err := json.Unmarshal(lines[index], &call); err != nil || call.Method != "logout" || call.Scope != want {
			t.Fatalf("wrong selected logout authority: %+v, %v", call, err)
		}
	}
}
