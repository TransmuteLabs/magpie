package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func extensionAuthUsage(t *testing.T, host *fixtureExtensionProviderHost, result json.RawMessage, callErr error) SubscriptionQuota {
	t.Helper()
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage {
			t.Fatalf("unexpected usage dispatch %s", method)
		}
		return result, callErr
	}
	logins, _ := extensionLogins(ExtensionProviderID(host.manifest.ID, "vendor"))
	return extensionQuota(context.Background(), logins[0])
}

func extensionLapseFor(t *testing.T, host *fixtureExtensionProviderHost, user string) string {
	t.Helper()
	for _, login := range Logins(ExtensionProviderID(host.manifest.ID, "vendor")) {
		if login.User == user {
			return login.Lapsed
		}
	}
	t.Fatalf("missing extension account %s", user)
	return ""
}

func TestExtensionUsageTypedAuthRefusalAndRecovery(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	quota := extensionAuthUsage(t, host, nil, &sdk.Error{Code: "unauthenticated", Message: "fixture credentials refused"})
	if quota.Error == "" || extensionLapseFor(t, host, "alice") == "" {
		t.Fatal("typed usage authentication refusal did not reach the ordinary login row")
	}
	if extensionLapseFor(t, host, "bob") != "" {
		t.Fatal("one usage refusal lapsed a sibling account")
	}
	selected := extensionAccounts()[0]
	if selected.SignInNeeded() {
		t.Fatal("healthy sibling account was treated as signed out")
	}
	if err := SetLoginOn(selected.ID, "bob", false); err != nil {
		t.Fatal(err)
	}
	if !selected.SignInNeeded() {
		t.Fatal("ordinary sign-in-required policy missed the selected usage refusal")
	}
	extensionAuthUsage(t, host, nil, errors.New("HTTP 401 Unauthorized"))
	if extensionLapseFor(t, host, "alice") == "" {
		t.Fatal("an untyped failed read cleared a proven refusal")
	}
	extensionAuthUsage(t, host, json.RawMessage(`{"usage":{},"windows":[{"name":"Plan","used":12}]}`), nil)
	if extensionLapseFor(t, host, "alice") != "" {
		t.Fatal("validated successful usage did not clear its matching refusal")
	}
	extensionAuthUsage(t, host, nil, errors.New("HTTP 401 Unauthorized; sign in again"))
	if extensionLapseFor(t, host, "alice") != "" {
		t.Fatal("untyped error prose changed authentication state")
	}
}

func TestExtensionUsageAuthOnlyFreshResultClearsRefusal(t *testing.T) {
	for _, minor := range []int{0, 3, 12} {
		for _, sample := range []struct {
			name string
			body string
		}{
			{"historical result", `{"usage":{},"asOf":1,"windows":[{"name":"Plan","used":12}]}`},
			{"historical window", `{"usage":{},"windows":[{"name":"Plan","used":12,"asOf":1}]}`},
			{"unknown window", `{"usage":{},"windows":[{"name":"Plan"}]}`},
			{"window failure", `{"usage":{},"windows":[{"name":"Plan","error":"unavailable"}]}`},
			{"empty windows", `{"usage":{},"windows":[]}`},
		} {
			t.Run(fmt.Sprintf("api1.%d/%s", minor, sample.name), func(t *testing.T) {
				host := fixtureExtensionProvider(t, true)
				host.manifest.Contract.Version.Minor = minor
				extensionAuthUsage(t, host, nil, &sdk.Error{Code: "unauthenticated", Message: "refused"})
				if extensionLapseFor(t, host, "alice") == "" {
					t.Fatal("fixture did not establish an authentication refusal")
				}
				host.call = func(_ context.Context, method string, _ sdk.Scope, request any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
					if method != sdk.MethodProviderUsage {
						t.Fatalf("unexpected method %s", method)
					}
					var result map[string]any
					if err := json.Unmarshal([]byte(sample.body), &result); err != nil {
						t.Fatal(err)
					}
					if minor >= 4 {
						result["revision"] = request.(sdk.UsageRequest).Revision
					}
					raw, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					if _, fresh, err := sdk.ValidateUsageResult(host.manifest, raw); err != nil || fresh {
						t.Fatalf("fixture must be valid non-fresh usage: fresh=%v err=%v", fresh, err)
					}
					return raw, nil
				}
				logins, _ := extensionLogins(ExtensionProviderID(host.manifest.ID, "vendor"))
				quota := extensionQuota(t.Context(), logins[0])
				if quota.Error != "" || extensionLapseFor(t, host, "alice") == "" {
					t.Fatalf("non-fresh usage erased a proven authentication refusal: error=%q", quota.Error)
				}
			})
		}
	}
}

func TestExtensionUsageAuthUnavailableLockDefersStateChange(t *testing.T) {
	for _, completion := range []bool{false, true} {
		for _, refused := range []bool{false, true} {
			t.Run(fmt.Sprintf("completion-%v/refused-%v", completion, refused), func(t *testing.T) {
				host := fixtureExtensionProvider(t, true)
				if !refused {
					extensionAuthUsage(t, host, nil, &sdk.Error{Code: "unauthenticated", Message: "refused"})
				}
				logins, _ := extensionLogins(ExtensionProviderID(host.manifest.ID, "vendor"))
				before, err := os.ReadFile(loginsPath())
				if err != nil {
					t.Fatal(err)
				}
				blockLock := func() {
					if err := os.Remove(loginsPath() + ".lock"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(loginsPath()+".lock", 0700); err != nil {
						t.Fatal(err)
					}
				}
				if !completion {
					blockLock()
				}
				dispatched := 0
				host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
					if method != sdk.MethodProviderUsage {
						t.Fatalf("unexpected method %s", method)
					}
					dispatched++
					if completion {
						before, err = os.ReadFile(loginsPath())
						if err != nil {
							t.Fatal(err)
						}
						blockLock()
					}
					if refused {
						return nil, &sdk.Error{Code: "unauthenticated", Message: "refused"}
					}
					return json.RawMessage(`{"usage":{},"windows":[{"name":"Plan","used":12}]}`), nil
				}
				quota := extensionQuota(t.Context(), logins[0])
				if !quota.ReadDeferred || quota.Error == "" || dispatched != map[bool]int{false: 0, true: 1}[completion] {
					t.Errorf("unlocked read was not deferred: quota=%+v dispatched=%d", quota, dispatched)
				}
				after, err := os.ReadFile(loginsPath())
				if err != nil || string(after) != string(before) {
					t.Fatalf("unavailable lock allowed an authentication-state write: err=%v", err)
				}
			})
		}
	}
}

func TestExtensionUsageAuthLatestReadOwnsPublication(t *testing.T) {
	for _, newerRefusal := range []bool{false, true} {
		t.Run(map[bool]string{false: "newer recovery", true: "newer refusal"}[newerRefusal], func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			logins, _ := extensionLogins(ExtensionProviderID(host.manifest.ID, "vendor"))
			selected := logins[0]
			var calls atomic.Int32
			clean := json.RawMessage(`{"usage":{},"windows":[{"name":"Plan","used":12}]}`)
			refused := &sdk.Error{Code: "unauthenticated", Message: "refused"}
			host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
				if method != sdk.MethodProviderUsage {
					t.Fatalf("unexpected method %s", method)
				}
				if calls.Add(1) == 1 {
					extensionQuota(context.Background(), selected)
					if newerRefusal {
						return clean, nil
					}
					return nil, refused
				}
				if newerRefusal {
					return nil, refused
				}
				return clean, nil
			}
			extensionQuota(context.Background(), selected)
			if calls.Load() != 2 || (extensionLapseFor(t, host, "alice") != "") != newerRefusal {
				t.Fatalf("older usage result replaced newer authentication state: calls=%d lapsed=%q",
					calls.Load(), extensionLapseFor(t, host, "alice"))
			}
		})
	}
}

func TestExtensionUsageAuthKeepAliveUsesCurrentSelection(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 12
	host.manifest.Providers[0].KeepAlive = &sdk.AccountKeepAliveDescriptor{}
	account, err := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	account.ExpiresAt = time.Now().Add(2 * time.Hour).UnixMilli()
	account, err = host.store.SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	extensionAuthUsage(t, host, nil, &sdk.Error{Code: "unauthenticated", Message: "refused"})
	selected, err := extensionSignInsKept(context.Background())
	if err != nil || len(selected) != 0 {
		t.Fatalf("idle keepalive renewed a proven refused sign-in: selected=%d err=%v", len(selected), err)
	}
	account.Auth = json.RawMessage(`{"access":"new"}`)
	if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	selected, err = extensionSignInsKept(context.Background())
	if err != nil || len(selected) != 1 || selected[0].scope.Generation != account.Generation+1 {
		t.Fatalf("idle keepalive inherited an old credential refusal: selected=%+v err=%v", selected, err)
	}
}

func TestExtensionUsageAuthMalformedMarkKeepsKnownRefusal(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 12
	host.manifest.Providers[0].KeepAlive = &sdk.AccountKeepAliveDescriptor{}
	account, err := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	account.ExpiresAt = time.Now().Add(2 * time.Hour).UnixMilli()
	if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtensionProviders(); err != nil {
		t.Fatal(err)
	}
	loginsMu.Lock()
	logins, err := readLoginsFile()
	if err == nil {
		for index := range logins {
			if logins[index].User == "alice" {
				logins[index].Lapsed = "Sign in again"
				logins[index].Refused = extensionAuthMark + "broken"
			}
		}
		err = writeLogins(logins)
	}
	loginsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if extensionLapseFor(t, host, "alice") == "" {
		t.Fatal("malformed metadata silently cleared a known sign-in refusal")
	}
	selected, err := extensionSignInsKept(context.Background())
	if err != nil || len(selected) != 0 {
		t.Fatalf("malformed metadata authorized idle authentication: %d %v", len(selected), err)
	}
}

func TestExtensionUsageAuthThroughInstalledWorker(t *testing.T) {
	fixture := keepAliveOwnerFixture(t, nil)
	path := filepath.Join(fixture.directory, sdk.ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Contract.Version.Minor = 12
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.UsageReporter)
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Install(t.Context(), fixture.directory); err != nil {
		t.Fatal(err)
	}
	account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
	if err != nil {
		t.Fatal(err)
	}
	account.ExpiresAt = time.Now().Add(2 * time.Hour).UnixMilli()
	account, err = fixture.manager.Store().SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	Changed()
	id := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	read := func(refused bool) {
		t.Helper()
		quota := LoginUsage(t.Context(), id)[fixture.scope.Account]
		logins := Logins(id)
		if len(logins) != 1 || (logins[0].Lapsed != "") != refused || (quota.Error != "") != refused {
			t.Fatalf("installed worker authentication state: logins=%+v quota=%+v", logins, quota)
		}
	}
	read(true)
	account.Auth = json.RawMessage(`{"token":"synthetic-rotated"}`)
	if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	Changed()
	read(false)
}

func TestExtensionUsageRefusalCannotMarkReplacementSelection(t *testing.T) {
	for _, scenario := range []string{"generation", "configuration", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			host.manifest.Contract.Version.Minor = 12
			logins, _ := extensionLogins(ExtensionProviderID(host.manifest.ID, "vendor"))
			selected := logins[0]
			host.call = func(_ context.Context, _ string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
				switch scenario {
				case "generation":
					account, err := host.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
					if err != nil {
						t.Fatal(err)
					}
					account.Auth = json.RawMessage(`{"access":"replacement"}`)
					if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
						t.Fatal(err)
					}
				case "configuration":
					host.config = "1"
				case "disabled":
					host.disabled = true
				}
				return nil, &sdk.Error{Code: "unauthenticated", Message: "old credentials refused"}
			}
			quota := extensionQuota(context.Background(), selected)
			if quota.Error == "" {
				t.Fatal("refused usage became a successful quota")
			}
			if scenario == "disabled" {
				host.disabled = false
			}
			if extensionLapseFor(t, host, "alice") != "" {
				t.Fatal("old usage refusal marked the replacement selection")
			}
		})
	}
}

func TestExtensionUsageRefusalDoesNotSurviveNewCredentials(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	extensionAuthUsage(t, host, nil, &sdk.Error{Code: "unauthenticated", Message: "refused"})
	if extensionLapseFor(t, host, "alice") == "" {
		t.Fatal("fixture never marked the refused credential")
	}
	account, err := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	account.Auth = json.RawMessage(`{"access":"new"}`)
	if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	Changed()
	if got := extensionLapseFor(t, host, "alice"); got != "" {
		t.Fatalf("new credentials inherited an old usage refusal: %s", got)
	}
	raw, err := readLoginsFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range raw {
		auth := strings.TrimSpace(string(record.Auth))
		if strings.Contains(record.Lapsed, "fixture credentials") || auth != "" && auth != "null" {
			t.Fatal("usage authentication state copied credentials or vendor details into login metadata")
		}
	}
}
