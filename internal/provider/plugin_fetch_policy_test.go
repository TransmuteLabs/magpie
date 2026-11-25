package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

const fixturePluginChatURL = pluginBase + destinationPlugin + "/v1/chat/completions"

func TestPluginFetchChecksCurrentPolicyAfterHeaders(t *testing.T) {
	for _, change := range []string{"account paused", "account hidden", "discovery off", "policy unreadable", "signed out"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			var calls atomic.Int32
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			hook, entered, release := holdDestinationSetup(t, ctx)
			t.Setenv("DESTINATION_HEADERS", hook)
			signIn := destinationSignIn("fixture-key", vendor.URL)
			p := destinationProvider(t, ctx, vendor.URL, signIn)
			if change == "account paused" {
				if err := plugin.Restore(ctx, destinationPlugin, destinationPlugin+"#backup", signIn); err != nil {
					t.Fatal(err)
				}
			}
			want := ""
			wait := startDestinationFetch(t, ctx, p, "held-headers", entered)
			switch change {
			case "account paused":
				if err := SetLoginOn("plugin:"+destinationPlugin, p.Account.User, false); err != nil {
					t.Fatal(err)
				}
				want = "switched off"
			case "account hidden":
				saveLogins(t, savedLogin{
					Agent: "plugin:" + destinationPlugin, User: p.Account.User, Home: destinationPlugin, On: true, Hidden: "fixture-hidden",
				})
				want = "hidden"
			case "discovery off":
				saved := settings.Load()
				saved.NoAccountDiscovery = true
				if err := settings.Save(saved); err != nil {
					t.Fatal(err)
				}
				want = errAccountDiscoveryOff.Error()
			case "policy unreadable":
				if err := os.WriteFile(loginsPath(), []byte("invalid logins"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = "account policy"
			case "signed out":
				if err := plugin.SignOut(ctx, destinationPlugin, destinationPlugin); err != nil {
					t.Fatal(err)
				}
				want = "sign-in changed"
			}
			close(release)
			result := <-wait
			if result.err == nil || !strings.Contains(result.err.Error(), want) || calls.Load() != 0 {
				t.Fatalf("after %s, status = %d, error = %v, vendor calls = %d, want %q and no send",
					change, result.status, result.err, calls.Load(), want)
			}
		})
	}
}

func TestPluginFetchRequiresItsSelectedSavedSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var calls atomic.Int32
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	signIn := destinationSignIn("fixture-key", vendor.URL)
	p := destinationProvider(t, ctx, vendor.URL, signIn)
	if err := plugin.Restore(ctx, destinationPlugin, destinationPlugin+"#backup", signIn); err != nil {
		t.Fatal(err)
	}
	if err := plugin.SignOut(ctx, destinationPlugin, destinationPlugin); err != nil {
		t.Fatal(err)
	}
	status, err := doDestinationRequest(ctx, p, fixturePluginChatURL, "", destinationChat)
	if err == nil || !strings.Contains(err.Error(), "sign-in") || calls.Load() != 0 {
		t.Fatalf("missing selected slot gave status %d, error %v, vendor calls %d", status, err, calls.Load())
	}
	if err := plugin.Restore(ctx, destinationPlugin, destinationPlugin, signIn); err != nil {
		t.Fatal(err)
	}
	status, err = doDestinationRequest(ctx, p, fixturePluginChatURL, "", destinationChat)
	if err != nil || status != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("restored selected slot gave status %d, error %v, vendor calls %d", status, err, calls.Load())
	}
}

func TestPluginFetchKeepsLegacyLoginDefaults(t *testing.T) {
	for _, policy := range []string{"missing login", "explicit first", "implicit first"} {
		t.Run(policy, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			p := destinationProvider(t, ctx, vendor.URL, destinationSignIn("fixture-key", vendor.URL))
			switch policy {
			case "missing login":
				if err := os.Remove(loginsPath()); err != nil {
					t.Fatal(err)
				}
			default:
				saveLogins(t, savedLogin{
					Agent: "plugin:" + destinationPlugin, User: p.Account.User, Home: destinationPlugin, First: policy == "explicit first",
				})
			}
			status, err := doDestinationRequest(ctx, p, fixturePluginChatURL, "", destinationChat)
			if err != nil || status != http.StatusNoContent {
				t.Fatalf("legacy %s gave status %d, error %v", policy, status, err)
			}
		})
	}
}

func TestPluginFetchUsesCurrentLoaderOptions(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		name := "cached loader"
		if preparing {
			name = "late preparation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			routes := make(chan string, 4)
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				routes <- r.Header.Get("X-Loader-Route")
				w.WriteHeader(http.StatusNoContent)
			}))
			hook, entered, release := holdDestinationSetup(t, ctx)
			if preparing {
				t.Setenv("DESTINATION_LOADER_WAIT", hook)
			}
			p := destinationProvider(t, ctx, vendor.URL, destinationSignIn("fixture-key", vendor.URL))
			send := func(want string) {
				t.Helper()
				status, err := doDestinationRequest(ctx, p, fixturePluginChatURL, "", destinationChat)
				if err != nil || status != http.StatusNoContent {
					t.Fatalf("request with %s options gave status %d, error %v", want, status, err)
				}
				select {
				case route := <-routes:
					if route != want {
						t.Errorf("request used loader route %q, want %q", route, want)
					}
				default:
					t.Error("request did not reach the fixture")
				}
			}
			var pending <-chan destinationFetchResult
			if preparing {
				pending = startDestinationFetch(t, ctx, p, "", entered)
			} else {
				send("old")
			}
			saved := settings.Load()
			saved.PluginOptions = map[string]map[string]bool{destinationPlugin: {"new-route": true}}
			if err := settings.Save(saved); err != nil {
				t.Fatal(err)
			}
			if err := plugin.SetProviderOptions(ctx); err != nil {
				t.Fatal(err)
			}
			send("new")
			if preparing {
				close(release)
				result := <-pending
				if result.err == nil || !strings.Contains(result.err.Error(), "options changed") {
					t.Errorf("old preparation gave status %d, error %v, want an options change", result.status, result.err)
				}
				select {
				case route := <-routes:
					t.Errorf("old preparation reached the vendor with route %q", route)
				default:
				}
				send("new")
			}
		})
	}
}

type destinationFetchResult struct {
	status int
	err    error
}

func holdDestinationSetup(t *testing.T, ctx context.Context) (string, <-chan struct{}, chan struct{}) {
	t.Helper()
	path := "/prepare/" + os.Getenv("FAKE_RUN")
	entered, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			io.WriteString(w, "continue")
		case <-r.Context().Done():
		case <-ctx.Done():
		}
	}))
	t.Cleanup(server.Close)
	return server.URL + path, entered, release
}

func startDestinationFetch(t *testing.T, ctx context.Context, p Provider, session string, entered <-chan struct{}) <-chan destinationFetchResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	result := make(chan destinationFetchResult, 1)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		status, err := doDestinationRequest(ctx, p, fixturePluginChatURL, session, destinationChat)
		result <- destinationFetchResult{status, err}
	}()
	select {
	case <-entered:
		return result
	case early := <-result:
		t.Fatalf("fetch ended during preparation: %+v", early)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil
}

func TestPinnedTestAllowsSwitchedOffNotHidden(t *testing.T) {
	claudeHome(t)
	epoch := "epoch-1"
	saveLogins(t,
		savedLogin{Agent: "plugin:fakeco", User: "a@fake", Home: "fakeco", On: true, First: true},
		savedLogin{Agent: "plugin:fakeco", User: "b@fake", Home: "fakeco#2", On: false},
		savedLogin{Agent: "plugin:fakeco", User: "c@fake", Home: "fakeco#3", On: false, Hidden: "removed"},
		savedLogin{Agent: "ext:demo", User: "off@ext", Home: "off@ext", On: false},
		savedLogin{Agent: "ext:demo", User: "hidden@ext", Home: "hidden@ext", On: false, Hidden: "removed"},
	)
	authOff := plugin.FetchAuth{Account: "fakeco#2", Epoch: &epoch}
	if err := checkPluginFetchAccount(context.Background(), "fakeco", "fakeco#2", false, false, authOff); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("unpinned off account: err = %v", err)
	}
	wrongPinned := withPinnedTestAccount(context.Background(), &Account{
		plugin:    &plugin.Provider{ID: "fakeco"},
		pluginKey: "fakeco",
	})
	if err := checkPluginFetchAccount(wrongPinned, "fakeco", "fakeco#2", false, false, authOff); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("other account pinned: err = %v", err)
	}
	pinnedOff := withPinnedTestAccount(context.Background(), &Account{
		plugin:    &plugin.Provider{ID: "fakeco"},
		pluginKey: "fakeco#2",
	})
	if err := checkPluginFetchAccount(pinnedOff, "fakeco", "fakeco#2", false, false, authOff); err != nil {
		t.Fatalf("pinned off account refused: %v", err)
	}
	authHidden := plugin.FetchAuth{Account: "fakeco#3", Epoch: &epoch}
	pinnedHidden := withPinnedTestAccount(context.Background(), &Account{
		plugin:    &plugin.Provider{ID: "fakeco"},
		pluginKey: "fakeco#3",
	})
	if err := checkPluginFetchAccount(pinnedHidden, "fakeco", "fakeco#3", false, false, authHidden); err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("pinned hidden account allowed: err = %v", err)
	}

	extOff := extensionPublicIdentity{provider: "demo", policyAgent: "ext:demo", user: "off@ext"}
	if err := checkExtensionLoginForExecute(context.Background(), extOff); err == nil {
		t.Fatal("unpinned off extension account allowed")
	}
	pinnedExtOff := withPinnedTestAccount(context.Background(), &Account{
		extension: &extensionAccount{public: extOff},
	})
	if err := checkExtensionLoginForExecute(pinnedExtOff, extOff); err != nil {
		t.Fatalf("pinned off extension account refused: %v", err)
	}
	extHidden := extensionPublicIdentity{provider: "demo", policyAgent: "ext:demo", user: "hidden@ext"}
	pinnedExtHidden := withPinnedTestAccount(context.Background(), &Account{
		extension: &extensionAccount{public: extHidden},
	})
	if err := checkExtensionLoginForExecute(pinnedExtHidden, extHidden); err == nil {
		t.Fatal("pinned hidden extension account allowed")
	}
}
