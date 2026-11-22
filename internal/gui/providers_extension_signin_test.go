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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type providerSignInOptions struct {
	OwnerRevision   string `json:"ownerRevision"`
	PackageRevision string `json:"packageRevision"`
	ConfigVersion   string `json:"configVersion"`
	Options         []struct {
		Key string `json:"key"`
		On  bool   `json:"on"`
	} `json:"options"`
}

type providerSignInRow struct {
	providerJSON
	ExtensionOptions *providerSignInOptions `json:"extensionOptions"`
}

type providerSignInState struct {
	providersJSON
	Providers []providerSignInRow `json:"providers"`
}

func readProviderSignInState(t *testing.T, handler http.Handler) providerSignInState {
	t.Helper()
	response := do(handler, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	var state providerSignInState
	if response.Code != http.StatusOK {
		t.Fatalf("/api/providers returned %d", response.Code)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatalf("/api/providers returned invalid state: %v", err)
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
		state.SDKSubscriptions[0].Name != "Vendor" || state.SDKSubscriptions[0].Icon != "generic" || state.SDKSubscriptions[0].Page == "" {
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

func providerAuthFixtureManager(t *testing.T) *extension.Manager {
	t.Helper()
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager
}

func providerAuthFixtureManifest() sdk.Manifest {
	manifest := authfixture.Manifest()
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities,
		sdk.ProviderSignIn, sdk.ProviderSavedSignIn, sdk.OutboundExecutor, sdk.UsageReporter)
	manifest.Providers[0].Models = []sdk.Model{{ID: "fixture-model", Name: "Fixture Model"}}
	manifest.AuthBindings = append(manifest.AuthBindings, sdk.WindowAuthBindingDescriptor{
		ID: "provider.signin", From: sdk.ProviderSignInPart, Provider: "vendor", Reauthorize: true,
		Methods: []sdk.WindowAuthMethod{{ID: "device", Mode: "poll", Label: "Device code"}, {
			ID: "key", Mode: "poll", Label: "API key", Fields: []sdk.AuthInputField{
				{Key: "key", Label: "API key", Type: "secret"},
				{Key: "site", Label: "Site", Type: "select", Optional: true, Choices: []sdk.AuthInputChoice{{Value: "main", Label: "Main"}}},
			},
		}, {
			ID: sdk.SavedSignInMethod, Mode: "poll", Label: "Import saved sign-in",
			Fields: []sdk.AuthInputField{{Key: "record", Label: "Saved account", Type: "secret"}},
		}},
	})
	return manifest
}

func installProviderAuthFixture(t *testing.T, manager *extension.Manager, manifest *sdk.Manifest) string {
	t.Helper()
	return installProviderAuthFixtureWindow(t, manager, manifest, `{"name":"auth-fixture","magpie":{"name":"Fixture module","icon":"opencode","page":{"title":"Fixture","file":"index.html"},"panels":[{"slot":"sessions","title":"History fixture","file":"index.html"}]}}`)
}

func installProviderAuthFixtureWindow(t *testing.T, manager *extension.Manager, manifest *sdk.Manifest, metadata string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	source := t.TempDir()
	build := proc.CommandContext(testenv.DeadlineContext(t, 5*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(source, manifest.Entry), "../extension/testdata/authworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build auth fixture: %v %s", err, output)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		sdk.ManifestFile: string(raw),
		"package.json":   metadata,
		"index.html":     `<!doctype html>`,
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestLoginUsageEditorHonorsStoreDeadline(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := providerAuthFixtureManager(t)
	manifest := providerAuthFixtureManifest()
	installProviderAuthFixture(t, manager, &manifest)
	if _, err := manager.Store().SaveAccount(extension.Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"PRIVATE-CREDENTIAL"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	id := provider.ExtensionProviderID(manifest.ID, "vendor")
	path := "/api/login/usage?agent=" + id
	for _, name := range []string{"enabled", "switched-off"} {
		t.Run(name, func(t *testing.T) {
			if name == "switched-off" {
				if err := provider.Save(provider.Provider{ID: id, Off: true}); err != nil {
					t.Fatal(err)
				}
			}
			synctest.Test(t, func(t *testing.T) {
				if got, err := filelock.TryLock(lock); err != nil || !got {
					t.Fatal("hold SDK Store:", err)
				}
				release := sync.OnceFunc(func() { filelock.Unlock(lock) })
				defer release()
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- do(mux, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)) }()
				time.Sleep(2 * time.Second)
				synctest.Wait()
				select {
				case response := <-done:
					if response.Body.Len() != 0 {
						t.Error("canceled account editor received a quota payload")
					}
				default:
					release()
					<-done
					t.Error("account editor waited for the SDK Store after its caller expired")
				}
			})
		})
	}
	if err := provider.Save(provider.Provider{ID: id}); err != nil {
		t.Fatal(err)
	}
	response := do(mux, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"used":17`)) {
		t.Fatalf("account editor did not recover after the Store unlocked: %d %s", response.Code, response.Body.String())
	}
}

type loginUsageReplyContext struct {
	context.Context
	beforeReply func()
}

func (ctx loginUsageReplyContext) Err() error {
	ctx.beforeReply()
	return ctx.Context.Err()
}

func TestLoginUsageEditorRevalidatesBeforeReply(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := providerAuthFixtureManager(t)
	manifest := providerAuthFixtureManifest()
	installProviderAuthFixture(t, manager, &manifest)
	var chosen sdk.Scope
	for id, token := range map[string]string{"chosen": "PRIVATE-CREDENTIAL", "other": "PRIVATE-OTHER"} {
		saved, err := manager.Store().SaveAccount(extension.Account{
			Plugin: manifest.ID, Provider: "vendor", ID: id, Auth: json.RawMessage(`{"token":"` + token + `"}`),
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if id == "chosen" {
			chosen = sdk.Scope{Plugin: saved.Plugin, Provider: saved.Provider, Account: saved.ID, Generation: saved.Generation}
		}
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	path := "/api/login/usage?agent=" + provider.ExtensionProviderID(manifest.ID, "vendor")
	read := func(t *testing.T, ctx context.Context) map[string]provider.SubscriptionQuota {
		t.Helper()
		response := do(mux, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		var quotas map[string]provider.SubscriptionQuota
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &quotas) != nil {
			t.Fatalf("account editor returned %d: %s", response.Code, response.Body.String())
		}
		return quotas
	}
	warm := read(t, t.Context())
	if windows := warm["chosen"].Windows; len(windows) != 1 || windows[0].Used != 17 {
		t.Fatalf("initial SDK quota: %+v", warm)
	}
	if windows := warm["other"].Windows; len(windows) != 1 || windows[0].Used != 23 {
		t.Fatalf("initial neighboring SDK quota: %+v", warm)
	}
	check := func(t *testing.T, expire bool) {
		t.Helper()
		mutated := false
		ctx := loginUsageReplyContext{Context: context.Background(), beforeReply: sync.OnceFunc(func() {
			if expire {
				time.Sleep(13 * time.Second)
			}
			if _, err := extension.NewStore(manager.Namespace()).InvalidateUsage(chosen); err != nil {
				t.Fatal(err)
			}
			mutated = true
		})}
		quotas := read(t, ctx)
		if !mutated {
			t.Fatal("the handler never reached the reply boundary")
		}
		changed, found := quotas["chosen"]
		if expire && found || !expire && (!found || len(changed.Windows) != 0 || !changed.ReadDeferred) {
			t.Errorf("account editor exposed the invalidated quota: %+v", changed)
		}
		if windows := quotas["other"].Windows; len(windows) != 1 || windows[0].Used != 23 {
			t.Errorf("account editor lost the valid neighbor: %+v", quotas["other"])
		}
	}
	t.Run("within budget", func(t *testing.T) { check(t, false) })
	t.Run("expired budget", func(t *testing.T) {
		if q := read(t, t.Context())["chosen"]; len(q.Windows) != 1 || q.Windows[0].Used != 17 {
			t.Fatalf("fixture could not refresh the invalidated quota: %+v", q)
		}
		synctest.Test(t, func(t *testing.T) { check(t, true) })
	})
}

func TestLoginUsageEditorRevalidatesLegacyOwnership(t *testing.T) {
	active := `[{"module":"devin-fixture","provider":"devin","publicID":"devin","revision":"fixture-cutover","active":true,
		"accounts":[{"key":"legacy","id":"sdk-account","user":"alice","loginID":"saved-login"}],
		"grantRevision":"` + strings.Repeat("a", 64) + `","digests":{"legacy":"` + strings.Repeat("b", 64) + `"},"tickets":["ticket"]}]`
	for _, scenario := range []struct {
		name, record string
		cutover      bool
		expire       bool
	}{
		{name: "active owner", record: active, cutover: true},
		{name: "active owner after budget", record: active, cutover: true, expire: true},
		{name: "malformed owners", record: `[{"publicID":"devin"}]`},
		{name: "unreadable owners"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			sandboxHome(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			manager := providerAuthFixtureManager(t)
			ownersPath := filepath.Join(manager.Namespace(), "private", ".saved-signins", "subscription-owners.json")
			reads := 0
			provider.LoginUsageVia(func(context.Context, string) map[string]provider.SubscriptionQuota {
				reads++
				return map[string]provider.SubscriptionQuota{
					"alice": {
						Provider: "devin", User: "alice", Name: "Devin",
						Windows: []provider.QuotaWindow{{Name: "Weekly", Used: 41}},
						Balance: "$5", Renew: "auto", Resets: &provider.ResetCredits{Count: 2},
					},
					"other": {Provider: "copilot", User: "other", Windows: []provider.QuotaWindow{{Name: "Weekly", Used: 27}}},
				}
			})
			t.Cleanup(func() { provider.LoginUsageVia(nil) })
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			read := func(t *testing.T, ctx context.Context) map[string]provider.SubscriptionQuota {
				t.Helper()
				response := do(mux, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/login/usage?agent=plugin:devin", nil))
				var quotas map[string]provider.SubscriptionQuota
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &quotas) != nil {
					t.Fatalf("account editor returned %d: %s", response.Code, response.Body.String())
				}
				return quotas
			}
			if quota := read(t, t.Context())["alice"]; len(quota.Windows) != 1 || quota.Windows[0].Used != 41 {
				t.Fatalf("initial legacy quota: %+v", quota)
			}
			check := func(t *testing.T) {
				t.Helper()
				mutated := false
				ctx := loginUsageReplyContext{Context: context.Background(), beforeReply: sync.OnceFunc(func() {
					if reads != 2 {
						t.Fatal("ownership changed before the legacy read completed")
					}
					if scenario.expire {
						time.Sleep(13 * time.Second)
					}
					if scenario.record == "" {
						if err := os.MkdirAll(ownersPath, 0700); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.MkdirAll(filepath.Dir(ownersPath), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(ownersPath, []byte(scenario.record), 0600); err != nil {
							t.Fatal(err)
						}
					}
					owners, err := extension.NewStore(manager.Namespace()).SubscriptionOwners(t.Context())
					if scenario.cutover {
						if err != nil || len(owners) != 1 || !owners[0].Active || owners[0].PublicID != "devin" {
							t.Fatalf("fixture did not activate the public owner: %+v, %v", owners, err)
						}
					} else if err == nil {
						t.Fatal("fixture ownership file was accepted")
					}
					mutated = true
				})}
				quotas := read(t, ctx)
				if !mutated || reads != 2 {
					t.Fatalf("quota read count %d after publication, changed owner: %t", reads, mutated)
				}
				quota, found := quotas["alice"]
				if scenario.expire {
					if found {
						t.Errorf("partial reply kept the superseded quota: %+v", quota)
					}
				} else if !found || quota.Name != "Devin" || quota.User != "alice" || !quota.ReadDeferred || quota.Error == "" ||
					len(quota.Windows) != 0 || quota.Balance != "" || quota.Resets != nil || quota.Renew != "" {
					t.Errorf("account editor exposed an unverified legacy quota: %+v", quota)
				}
				neighbor := quotas["other"]
				if scenario.cutover {
					if len(neighbor.Windows) != 1 || neighbor.Windows[0].Used != 27 || neighbor.ReadDeferred {
						t.Errorf("account editor lost the verified neighbor: %+v", neighbor)
					}
				} else if !neighbor.ReadDeferred || len(neighbor.Windows) != 0 {
					t.Errorf("account editor verified a local quota with unreadable owners: %+v", neighbor)
				}
			}
			if scenario.expire {
				synctest.Test(t, check)
			} else {
				check(t)
			}
		})
	}
}

func TestLoginUsageEditorReturnsCompletedRowsAtDeadline(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	for _, scenario := range []struct {
		name          string
		completed     bool
		callerTimeout bool
		code          int
	}{
		{name: "partial", completed: true, code: http.StatusOK},
		{name: "empty", code: http.StatusGatewayTimeout},
		{name: "caller canceled", completed: true, callerTimeout: true, code: http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			provider.LoginUsageVia(func(ctx context.Context, _ string) map[string]provider.SubscriptionQuota {
				<-ctx.Done()
				if !scenario.completed {
					return nil
				}
				return map[string]provider.SubscriptionQuota{
					"ready": {Provider: "copilot", User: "ready", Windows: []provider.QuotaWindow{{Name: "Weekly", Used: 27}}},
				}
			})
			t.Cleanup(func() { provider.LoginUsageVia(nil) })
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if scenario.callerTimeout {
					parent, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					ctx = parent
				}
				response := do(mux, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/login/usage?agent=copilot", nil))
				if response.Code != scenario.code {
					t.Fatalf("account editor returned %d: %s", response.Code, response.Body.String())
				}
				if scenario.callerTimeout {
					if response.Body.Len() != 0 {
						t.Errorf("canceled client received %s", response.Body.String())
					}
				} else if scenario.completed {
					var quotas map[string]provider.SubscriptionQuota
					if err := json.Unmarshal(response.Body.Bytes(), &quotas); err != nil || len(quotas) != 1 {
						t.Fatalf("invalid partial reply: %s", response.Body.String())
					}
					if windows := quotas["ready"].Windows; len(windows) != 1 || windows[0].Used != 27 {
						t.Errorf("partial reply lost the completed quota: %+v", quotas)
					}
				}
			})
		})
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
	manager := providerAuthFixtureManager(t)
	manifest := providerAuthFixtureManifest()
	source := installProviderAuthFixture(t, manager, &manifest)
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
	signIn(selected().Revision, "saved-import", "start",
		`"request":"start","options":{"method":"saved-signin","input":{"record":"PRIVATE-SAVED"}}`, false)
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
	raw, err := json.Marshal(manifest)
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
	policyPath := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	unreadablePolicy := []byte("{")
	if err := os.WriteFile(policyPath, unreadablePolicy, 0o600); err != nil {
		t.Fatal(err)
	}
	finish(current, "first-account", view, "1")
	if data, err := os.ReadFile(policyPath); err != nil || !bytes.Equal(data, unreadablePolicy) {
		t.Fatalf("completed sign-in overwrote unreadable account policy: %s (%v)", data, err)
	}
	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	finish(current, "first-account", view, "1")
	checkPolicy := func(on bool) {
		t.Helper()
		data, err := os.ReadFile(policyPath)
		var policies []struct {
			Agent, User string
			On          bool
		}
		if err != nil || json.Unmarshal(data, &policies) != nil {
			t.Fatalf("completed SDK sign-in has no readable account policy: %v", err)
		}
		count := 0
		for _, row := range policies {
			if row.Agent == id && row.User == "chosen" {
				count++
				if row.On != on {
					t.Fatalf("replayed sign-in changed account policy to on=%v", row.On)
				}
			}
		}
		if count != 1 {
			t.Fatalf("completed SDK sign-in registered %d matching policies, want 1", count)
		}
	}
	checkPolicy(true)
	if err := provider.SetLoginOn(id, "chosen", false); err != nil {
		t.Fatal(err)
	}
	finish(current, "first-account", view, "1")
	checkPolicy(false)
	if err := provider.SetLoginOn(id, "chosen", true); err != nil {
		t.Fatal(err)
	}
	state := readProviderSignInState(t, mux)
	found := false
	for _, row := range state.Providers {
		if row.ID == id {
			found = row.Account != nil && row.Account.Agent == id && len(row.Models) == 1 &&
				row.Models[0].ID == "fixture-model" && len(row.Account.Logins) == 1 &&
				row.AccountUsers["chosen"] == "reader@example.test" && row.Account.Logins[0].User == "chosen" && row.Account.Logins[0].ExtensionGeneration == "1"
			if row.Name != "Vendor" || row.Icon != "opencode" || row.Account == nil ||
				row.Account.Name != "Vendor" || row.Account.Icon != "opencode" {
				t.Error("signed-in SDK account did not keep its provider name and package icon")
			}
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
	if !slices.ContainsFunc(state.Providers, func(row providerSignInRow) bool {
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
	if slices.ContainsFunc(readProviderSignInState(t, mux).Providers, func(row providerSignInRow) bool { return row.ID == id }) ||
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

func TestTransferredSubscriptionKeepsOrdinaryIdentity(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_BUN", filepath.Join(t.TempDir(), "no-bun"))
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	manager := providerAuthFixtureManager(t)
	manifest := providerAuthFixtureManifest()
	const id = "devin"
	const spec = "@magpie-community/opencode-devin-auth"
	const saved = `{"devin":{"type":"api","key":"PRIVATE-SAVED"}}`
	manifest.Contract.Version.Minor = 9
	manifest.Providers[0].ID = id
	manifest.Providers[0].Name = "Fixture Devin"
	manifest.Providers[0].Options = []sdk.ProviderOption{{Key: "paid", Label: "Paid"}}
	for index := range manifest.AuthBindings {
		manifest.AuthBindings[index].Provider = id
	}
	installProviderAuthFixture(t, manager, &manifest)
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"plugins.json":     `{"plugins":[{"spec":"` + spec + `"}]}`,
		"plugin-auth.json": saved,
		"migrations.json":  `{"devin":{"state":"plugin","package":"` + spec + `"}}`,
	} {
		if err := os.WriteFile(filepath.Join(settings.Dir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plugin.UseCached([]plugin.Provider{{ID: id, Name: "Community Devin", Spec: spec, Methods: []plugin.Method{{Type: "api", Label: "API key"}}}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	provider.Changed()

	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	assertOwner := func(publicID, ownerRevision, account string, generation string) extensionSubscriptionJSON {
		t.Helper()
		state := readProviderSignInState(t, mux)
		var grant extensionSubscriptionJSON
		if publicID == "" {
			if len(state.SDKSubscriptions) != 0 {
				t.Fatalf("inactive SDK provider has an Add tile: %+v", state.SDKSubscriptions)
			}
		} else {
			if len(state.SDKSubscriptions) != 1 {
				t.Fatalf("wrong number of SDK grants: %+v", state.SDKSubscriptions)
			}
			grant = state.SDKSubscriptions[0]
			if grant.ID != publicID || grant.OwnerRevision != ownerRevision || !grant.Reauthorize || len(grant.Methods) != 2 {
				t.Fatalf("unexpected SDK grant: %+v", grant)
			}
		}
		community := ownerRevision == ""
		if slices.ContainsFunc(state.Plugins, func(sub pluginSubJSON) bool { return sub.ID == id }) != community ||
			slices.Contains(state.SDKOwnedSubscriptions, id) == community {
			t.Fatalf("Community and SDK availability disagree with owner revision %q", ownerRevision)
		}
		publicRows := 0
		for _, row := range state.Providers {
			if row.ID != id {
				continue
			}
			publicRows++
			if row.Account == nil || row.Account.OwnerRevision != ownerRevision || len(row.Account.Logins) != 1 ||
				row.Account.Logins[0].User != account || row.Account.Logins[0].ExtensionGeneration != generation {
				t.Fatalf("expected the same public account at generation %q: %+v", generation, row.Account)
			}
			if !community && (row.Name != grant.Name || row.Icon != grant.Icon || row.ExtensionOptions == nil ||
				row.ExtensionOptions.OwnerRevision != ownerRevision || len(row.Models) != 1) {
				t.Fatalf("SDK owner did not project its provider presentation and options: %+v", row)
			}
		}
		if publicRows != 1 {
			t.Fatalf("found %d rows for public id %q", publicRows, id)
		}
		var installed pluginsJSON
		response := do(mux, httptest.NewRequest(http.MethodGet, "/api/plugins", nil))
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &installed) != nil {
			t.Fatalf("Installed: %d %s", response.Code, response.Body)
		}
		sdkRow := slices.IndexFunc(installed.Plugins, func(row pluginEntryJSON) bool { return row.Spec == "sdk:"+manifest.ID })
		legacyRow := slices.IndexFunc(installed.Plugins, func(row pluginEntryJSON) bool { return row.Spec == spec })
		if sdkRow < 0 || legacyRow < 0 || installed.Plugins[sdkRow].SDK == nil ||
			slices.Contains(installed.Plugins[sdkRow].SDK.ProviderIDs, id) == community ||
			slices.Contains(installed.Plugins[legacyRow].Providers, "Community Devin") != community ||
			len(installed.Plugins[sdkRow].Panels) != 1 {
			t.Fatal("Installed lost the SDK panel or advertised the wrong provider owner")
		}
		return grant
	}
	post := func(path string, body any, accepted bool) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reply := do(mux, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
		if (reply.Code == http.StatusOK) != accepted || bytes.Contains(reply.Body.Bytes(), []byte("PRIVATE-")) {
			t.Fatalf("%s: HTTP %d %s", path, reply.Code, reply.Body)
		}
		return reply
	}
	logins := provider.Logins(id)
	if len(logins) != 1 || logins[0].User == "" || logins[0].User == "chosen" {
		t.Fatalf("fixture must have a distinct public login: %+v", logins)
	}
	user := logins[0].User
	hashed := provider.ExtensionProviderID(manifest.ID, id)
	original := assertOwner(hashed, "", user, "")
	transfer := func(before string) extension.SubscriptionOwner {
		t.Helper()
		prepared, err := provider.ImportSavedPluginAuth(t.Context(), manager, manifest.ID, id, id)
		if err != nil {
			t.Fatal(err)
		}
		assertOwner(before, "", user, "")
		owner, err := provider.TransferSavedPluginSubscription(t.Context(), manager, manifest.ID, id, []string{prepared.Ticket})
		if err != nil || owner.Revision == "" {
			t.Fatalf("activate transfer: %v", err)
		}
		return owner
	}
	owner := transfer(hashed)
	active := assertOwner(id, owner.Revision, user, "1")
	post("/api/plugin-signin/prompt", map[string]any{"provider": id}, false)
	post("/api/plugin-signin", map[string]any{"provider": id, "key": "invalid"}, false)
	post("/api/signin/import", map[string]any{"agent": id, "files": []string{`{"type":"api"}`}}, false)
	settingsHandler := Handler(nil, nil)
	legacyOption := func(providerID string, accepted bool) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"provider": providerID, "key": "paid", "on": true})
		if err != nil {
			t.Fatal(err)
		}
		reply := do(settingsHandler, httptest.NewRequest(http.MethodPost, "/api/settings/plugin-option", bytes.NewReader(raw)))
		if (reply.Code == http.StatusOK) != accepted ||
			settings.Load().PluginOptions[providerID]["paid"] != accepted {
			t.Fatalf("legacy provider switch %q accepted=%t: HTTP %d", providerID, accepted, reply.Code)
		}
	}
	legacyOption(id, false)
	legacyOption("other-provider", true)
	post("/api/provider/forget", map[string]any{"id": id}, false)
	post("/api/login/forget", map[string]any{"agent": id, "user": user}, false)
	post("/api/login/forget", map[string]any{"agent": id, "account": sdk.AccountSelection{ID: user, ExpectedGeneration: "1"}}, false)
	post("/api/login/forget", map[string]any{"agent": id, "ownerRevision": owner.Revision,
		"account": sdk.AccountSelection{ID: user, ExpectedGeneration: "2"}}, false)

	request := func(row extensionSubscriptionJSON, op string, fields map[string]any, accepted bool) sdk.WindowAuthFlowView {
		t.Helper()
		input := map[string]any{"id": row.ID, "revision": row.Revision, "ownerRevision": row.OwnerRevision, "parent": "fixture", "operation": op}
		for key, value := range fields {
			input[key] = value
		}
		reply := post("/api/provider/sdk-signin", input, accepted)
		var view sdk.WindowAuthFlowView
		if accepted && json.Unmarshal(reply.Body.Bytes(), &view) != nil {
			t.Fatalf("invalid public auth response: %s", reply.Body)
		}
		return view
	}
	start := func(row extensionSubscriptionJSON, account, generation, intent string, accepted bool) sdk.WindowAuthFlowView {
		t.Helper()
		return request(row, "start", map[string]any{"request": intent, "options": sdk.WindowAuthStartOptions{
			Method: "device", Account: &sdk.AccountSelection{ID: account, ExpectedGeneration: generation},
		}}, accepted)
	}
	finish := func(row extensionSubscriptionJSON, view sdk.WindowAuthFlowView, generation string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for view.State == "waiting" && time.Now().Before(deadline) {
			time.Sleep(time.Duration(view.RetryAfterMs) * time.Millisecond)
			view = request(row, "finish", map[string]any{"flow": view.Flow}, true)
		}
		if view.State != "complete" || view.Account == nil {
			t.Fatalf("sign-in did not complete: %s", view.State)
		}
		if view.Account.ID != user || view.Account.Generation != generation {
			t.Fatalf("sign-in returned %q at generation %s, want %q at generation %s",
				view.Account.ID, view.Account.Generation, user, generation)
		}
	}
	start(original, user, "1", "old-owner", false)
	stale := active
	stale.OwnerRevision = ""
	start(stale, user, "1", "missing-owner", false)
	start(active, "chosen", "1", "private-id", false)
	var options *providerSignInOptions
	for _, row := range readProviderSignInState(t, mux).Providers {
		if row.ID == id {
			options = row.ExtensionOptions
		}
	}
	if options == nil || len(options.Options) != 1 || options.Options[0].Key != "paid" || options.Options[0].On {
		t.Fatal("the active provider lost its ordinary options")
	}
	edit := map[string]any{"id": id, "module": manifest.ID, "provider": id, "packageRevision": options.PackageRevision,
		"expected": options.ConfigVersion, "values": map[string]bool{"paid": true}}
	post("/api/provider/extension-options", edit, false)
	edit["ownerRevision"] = owner.Revision
	post("/api/provider/extension-options", edit, true)
	fresh := assertOwner(id, owner.Revision, user, "1")
	if fresh.Revision == active.Revision {
		t.Fatal("a configuration change did not invalidate the open sign-in form")
	}
	start(active, user, "1", "old-config", false)
	if _, err := provider.RollbackSavedPluginSubscription(t.Context(), manager, id, owner.Revision); err != nil {
		t.Fatal(err)
	}
	assertOwner("", "", user, "")
	start(fresh, user, "1", "rolled-back-view", false)
	post("/api/login/forget", map[string]any{"agent": id, "ownerRevision": owner.Revision,
		"account": sdk.AccountSelection{ID: user, ExpectedGeneration: "1"}}, false)
	post("/api/provider/forget", map[string]any{"id": id, "ownerRevision": owner.Revision}, false)
	edit["expected"] = "1"
	post("/api/provider/extension-options", edit, false)
	legacyOption(id, true)

	owner = transfer("")
	active = assertOwner(id, owner.Revision, user, "1")
	start(fresh, user, "1", "previous-owner", false)
	finish(active, start(active, user, "1", "renew", true), "2")
	assertOwner(id, owner.Revision, user, "2")
	if _, err := provider.RollbackSavedPluginSubscription(t.Context(), manager, id, owner.Revision); err == nil {
		t.Fatal("rollback accepted after credential replacement")
	}
	post("/api/login/forget", map[string]any{"agent": id, "ownerRevision": active.OwnerRevision,
		"account": sdk.AccountSelection{ID: user, ExpectedGeneration: "1"}}, false)
	post("/api/login/forget", map[string]any{"agent": id, "ownerRevision": active.OwnerRevision,
		"account": sdk.AccountSelection{ID: user, ExpectedGeneration: "2"}}, true)
	assertSignedOut := func() {
		t.Helper()
		empty := readProviderSignInState(t, mux)
		if len(empty.SDKSubscriptions) != 1 || empty.SDKSubscriptions[0].ID != id ||
			empty.SDKSubscriptions[0].OwnerRevision != owner.Revision ||
			slices.ContainsFunc(empty.Providers, func(row providerSignInRow) bool { return row.ID == id }) {
			t.Fatal("last sign-out did not leave just the public SDK sign-in choice")
		}
		if _, err := manager.Store().GetAccount(manifest.ID, id, "chosen"); err == nil {
			t.Fatal("ordinary sign-out retained the active SDK credentials")
		}
	}
	assertSignedOut()
	finish(active, request(active, "start", map[string]any{"request": "new-after-sign-out",
		"options": sdk.WindowAuthStartOptions{Method: "device"}}, true), "4")
	assertOwner(id, owner.Revision, user, "4")
	post("/api/provider/delete", map[string]any{"id": id}, true)
	excluded := readProviderSignInState(t, mux).Excluded
	if !slices.ContainsFunc(excluded, func(row excludedJSON) bool {
		return row.Provider == id && row.OwnerRevision == owner.Revision
	}) {
		t.Fatal("the removed provider did not carry its owner in the sign-out form")
	}
	post("/api/provider/forget", map[string]any{"id": id}, false)
	post("/api/provider/forget", map[string]any{"id": id, "ownerRevision": owner.Revision}, true)
	assertSignedOut()
	raw, err := os.ReadFile(filepath.Join(settings.Dir(), "plugin-auth.json"))
	if err != nil || string(raw) != saved {
		t.Fatal("transfer or rollback changed the Community credential record")
	}
}
