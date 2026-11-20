package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionKeepAliveEligibilityAndUnknownReads(t *testing.T) {
	for _, condition := range []string{
		"enabled", "absent", "older SDK", "unknown expiry", "not due", "module off", "account off",
		"hidden account", "lapsed", "paused", "unlisted", "provider off", "hidden provider",
		"discovery off", "unknown settings", "unknown providers", "unknown logins", "unknown store",
	} {
		t.Run(condition, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			host.manifest.Contract.Version.Minor = 8
			host.manifest.Providers[0].KeepAlive = &sdk.AccountKeepAliveDescriptor{}
			for _, id := range []string{"alice", "bob"} {
				account, err := host.store.GetAccount(host.manifest.ID, "vendor", id)
				if err != nil {
					t.Fatal(err)
				}
				account.ExpiresAt = time.Now().Add(30 * time.Second).UnixMilli()
				if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			}
			providers, err := ExtensionProviders()
			if err != nil {
				t.Fatal(err)
			}
			provider := providers[0].ID
			logins, err := readLoginsFile()
			if err != nil {
				t.Fatal(err)
			}
			for index := range logins {
				if logins[index].User == "bob" {
					logins[index].On = false
				}
			}
			writePolicy := true
			want, unknown := 0, false
			switch condition {
			case "enabled", "not due":
				want = 1
			case "absent":
				host.manifest.Providers[0].KeepAlive = nil
			case "older SDK":
				host.manifest.Contract.Version.Minor = 7
			case "module off":
				host.disabled = true
			case "unknown expiry":
				account, _ := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
				account.ExpiresAt = 0
				if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "account off", "hidden account", "lapsed", "paused":
				for index := range logins {
					if logins[index].User != "alice" {
						continue
					}
					switch condition {
					case "account off":
						logins[index].On = false
					case "hidden account":
						logins[index].Hidden = "manual"
					case "lapsed":
						logins[index].Lapsed = "vendor refused renewal"
					case "paused":
						logins[index].Paused = true
					}
				}
			case "unlisted":
				logins = nil
			case "provider off", "hidden provider":
				raw, _ := json.Marshal(file{Providers: []Provider{{ID: provider, Off: condition == "provider off", Hidden: condition == "hidden provider"}}})
				if err := os.WriteFile(Path(), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "discovery off":
				t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
				writePolicy = false
			case "unknown settings", "unknown providers", "unknown logins", "unknown store":
				unknown = true
			}
			if writePolicy {
				if err := writeLogins(logins); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "not due" {
				account, _ := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
				account.ExpiresAt = time.Now().Add(2 * time.Hour).UnixMilli()
				if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			}
			if unknown {
				path := map[string]string{
					"unknown settings": settings.Path(), "unknown providers": Path(),
					"unknown logins": loginsPath(),
					"unknown store":  filepath.Join(host.namespace, "private", host.manifest.ID, "accounts.json"),
				}[condition]
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"broken":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
				t.Fatal("eligibility inspection invoked a module")
				return nil, nil
			}
			selected, err := extensionSignInsKept(t.Context())
			if len(selected) != want || (err != nil) != unknown {
				t.Fatalf("idle policy %s: selected=%d err=%v; want=%d unknown=%t", condition, len(selected), err, want, unknown)
			}
			if len(selected) != 0 && condition == "not due" {
				if !selected[0].at.After(time.Now().Add(time.Hour)) {
					t.Fatal("future expiry was made due")
				}
				var keeper extensionSignInKeeper
				if wait := keeper.look(t.Context(), time.Now()); wait != extensionKeepAliveLookMost {
					t.Fatal("not-due account changed the bounded idle wait", wait)
				}
			}
		})
	}
}

func TestExtensionKeepAliveBackoffAndReplacement(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	now := time.Now()
	selected := []extensionSignIn{{
		host: host, entry: extensionhost.Installed{Revision: "fixture"}, config: "0",
		scope: sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1},
		at:    now.Add(-time.Second),
	}}
	beforeList, beforeKeep := extensionSignInsKept, keepExtensionSignedIn
	t.Cleanup(func() { extensionSignInsKept, keepExtensionSignedIn = beforeList, beforeKeep })
	extensionSignInsKept = func(context.Context) ([]extensionSignIn, error) { return selected, nil }
	calls := 0
	keepExtensionSignedIn = func(context.Context, extensionSignIn) error {
		calls++
		return errors.New("synthetic transient hold")
	}
	var keeper extensionSignInKeeper
	keeper.look(t.Context(), now)
	keeper.look(t.Context(), now.Add(30*time.Second))
	if calls != 1 {
		t.Fatal("idle renewal retried inside its first backoff", calls)
	}
	keeper.look(t.Context(), now.Add(time.Minute))
	keeper.look(t.Context(), now.Add(2*time.Minute))
	if calls != 2 {
		t.Fatal("idle renewal did not double its retry delay", calls)
	}
	selected[0].scope.Generation++
	keeper.look(t.Context(), now.Add(2*time.Minute))
	if calls != 3 || len(keeper.held) != 1 {
		t.Fatal("a replacement inherited the old generation's backoff", calls, keeper.held)
	}
	extensionSignInsKept = func(context.Context) ([]extensionSignIn, error) { return nil, errors.New("unknown catalog") }
	keeper.look(t.Context(), now.Add(3*time.Minute))
	if calls != 3 || len(keeper.held) != 1 {
		t.Fatal("an unknown catalog erased backoff or dispatched work")
	}
	extensionSignInsKept = func(context.Context) ([]extensionSignIn, error) { return nil, nil }
	keeper.look(t.Context(), now.Add(4*time.Minute))
	if len(keeper.held) != 0 {
		t.Fatal("removed account retained idle backoff")
	}
}

type emptyKeepAliveHost struct {
	extensionProviderHost
	catalog *extensionhost.Manager
	listed  chan struct{}
}

func (host emptyKeepAliveHost) ListContext(ctx context.Context) ([]extensionhost.Installed, error) {
	entries, err := host.catalog.ListContext(ctx)
	select {
	case host.listed <- struct{}{}:
	default:
	}
	return entries, err
}

func TestExtensionKeepAliveEmptyCatalogLifecycleIsInert(t *testing.T) {
	providerFileHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	listed := make(chan struct{}, 1)
	catalog := extensionhost.NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := catalog.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	before := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost {
		return emptyKeepAliveHost{catalog: catalog, listed: listed}
	}
	t.Cleanup(func() { providerExtensionHost = before })
	stop := KeepExtensionsSignedIn(t.Context())
	t.Cleanup(stop)
	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("idle lifecycle did not inspect the empty catalog")
	}
	stop()
	stop()
}
