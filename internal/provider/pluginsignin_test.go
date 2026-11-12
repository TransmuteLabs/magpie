package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

// A plugin's sign-in is followed like a built-in one: the page, then the
// code pasted back (a wrong one fails the sign-in, which starts again);
// done, its provider is there, and removing it signs out. A key signs in
// at once.
func TestPluginSignIn(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}

	in := map[string]string{"where": "work", "team": "blue"}
	wait := func(id string) SignInState {
		t.Helper()
		for range 100 {
			if st, _ := SignInStatus(id); st.State != "waiting" {
				return st
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the sign-in is still waiting")
		return SignInState{}
	}
	st, err := StartPluginSignIn("fakeco", 1, in)
	if err != nil || st.Agent != "fakeco" || st.State != "waiting" || !st.PasteCode || !strings.Contains(st.URL, "where=work") || st.Instructions != "Paste the code" {
		t.Fatalf("StartPluginSignIn = %+v, %v", st, err)
	}
	if err := SubmitSignInCallback(st.ID, " "); err == nil {
		t.Fatal("an empty code was taken")
	}
	if err := SubmitSignInCallback(st.ID, "bad"); err == nil {
		t.Fatal("a wrong code was taken")
	}
	if got := wait(st.ID); got.State != "failed" {
		t.Fatalf("after a wrong code: %+v", got)
	}
	if err := SubmitSignInCallback(st.ID, "good"); err == nil {
		t.Fatal("a failed sign-in took another code")
	}

	st, _ = StartPluginSignIn("fakeco", 1, in)
	if err := SubmitSignInCallback(st.ID, "good"); err != nil {
		t.Fatal(err)
	}
	if got := wait(st.ID); got.State != "done" || got.User != "blue@fake" {
		t.Fatalf("after the code: %+v", got)
	}
	if p, err := Find("fakeco"); err != nil || !p.IsPlugin() {
		t.Fatalf("Find = %+v, %v", p, err)
	}
	// removed, it is hidden as a built-in is, still signed in
	if err := Delete("fakeco"); err != nil {
		t.Fatal(err)
	}
	if !plugin.SignedIn("fakeco") {
		t.Fatal("removing the provider signed out")
	}
	if _, err := Find("fakeco"); err == nil {
		t.Fatal("fakeco is a provider after it was removed")
	}
	if h := Hidden(); len(h) != 1 || h[0].ID != "fakeco" {
		t.Fatalf("Hidden = %+v", h)
	}

	if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, "  ", SignInOptions{}); err == nil {
		t.Fatal("an empty key was taken")
	}
	if id, err := PluginAPIKey(ctx, "fakeco", 0, nil, "k1", SignInOptions{}); err != nil || id != "fakeco" {
		t.Fatalf("PluginAPIKey = %q, %v", id, err)
	}
	if _, err := Find("fakeco"); err != nil {
		t.Fatal(err)
	}
}

// A plugin's provider takes more than one account, as a built-in
// subscription does: each signed in beside the others, one first and the
// rest on behind it or off, any put first, and removing one signs it out.
func TestPluginAccounts(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	signIn := func(team string) {
		t.Helper()
		st, err := StartPluginSignIn("fakeco", 1, map[string]string{"where": "work", "team": team})
		if err != nil {
			t.Fatal(err)
		}
		if err := SubmitSignInCallback(st.ID, "good"); err != nil {
			t.Fatal(err)
		}
		if got, _ := SignInStatus(st.ID); got.State != "done" || got.User != team+"@fake" {
			t.Fatalf("signing in %s: %+v", team, got)
		}
	}
	users := func() []string {
		var out []string
		for _, l := range Logins("fakeco") {
			u := l.User
			if l.Active {
				u = "*" + u
			}
			if !l.On {
				u += " (off)"
			}
			out = append(out, u)
		}
		return out
	}
	first := func() string {
		t.Helper()
		p, err := Find("fakeco")
		if err != nil {
			t.Fatal(err)
		}
		return p.Account.User
	}
	also := func() []string {
		p, _ := Find("fakeco")
		var out []string
		for _, q := range p.AlsoOn() {
			out = append(out, q.Account.User+"@"+q.Account.pluginKey)
		}
		return out
	}

	signIn("blue")
	signIn("red")
	signIn("red") // the same account again: not listed twice
	if got := strings.Join(users(), ","); got != "*blue@fake,red@fake" {
		t.Fatalf("accounts %s", got)
	}
	if first() != "blue@fake" {
		t.Fatalf("first is %s", first())
	}
	if a := also(); len(a) != 1 || !strings.HasPrefix(a[0], "red@fake@fakeco#") {
		t.Fatalf("also on %v", a)
	}
	if err := SetLoginOn("fakeco", "red@fake", false); err != nil {
		t.Fatal(err)
	}
	if a := also(); len(a) != 0 {
		t.Fatalf("red is off but %v", a)
	}
	if err := SetLoginOn("fakeco", "blue@fake", false); err == nil {
		t.Fatal("the first account was turned off")
	}
	if err := SwitchLogin("fakeco", "red@fake"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(users(), ","); got != "*red@fake,blue@fake" || first() != "red@fake" {
		t.Fatalf("after putting red first: %s, first %s", got, first())
	}
	if a := also(); len(a) != 1 || a[0] != "blue@fake@fakeco" {
		t.Fatalf("also on %v", a)
	}
	if err := ForgetLogin("fakeco", "blue@fake"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(users(), ","); got != "*red@fake" {
		t.Fatalf("after removing blue: %s", got)
	}
	for _, pp := range plugin.Cached() {
		if len(pp.Accounts) != 1 || !strings.HasPrefix(pp.Accounts[0].Key, "fakeco#") {
			t.Fatalf("blue is still signed in: %+v", pp.Accounts)
		}
	}

	// two keys with nothing to tell them by are told apart by their slot
	if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, "k1", SignInOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, "k2", SignInOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, "sk-long-key-abcd", SignInOptions{}); err != nil {
		t.Fatal(err)
	}
	us := users()
	if len(us) != 4 || us[1] == us[2] || !strings.HasPrefix(us[1], "API key") || us[3] != "API key …abcd" {
		t.Fatalf("keys listed as %v", us)
	}

	// removing the provider hides it, as a built-in's, every account kept;
	// shown again, they are all there
	if err := Delete("fakeco"); err != nil {
		t.Fatal(err)
	}
	if _, err := Find("fakeco"); err == nil || !plugin.SignedIn("fakeco") {
		t.Fatalf("removed: %v, signed in %v", err, plugin.SignedIn("fakeco"))
	}
	if err := ShowAccount("fakeco"); err != nil {
		t.Fatal(err)
	}
	if got := users(); !slices.Equal(got, us) {
		t.Fatalf("shown again with %v, was %v", got, us)
	}
}

func TestPluginAPIKeyReplacesSelectedSlotAndKeepsNewAdd(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first-key-1111", "second-key-2222"} {
		if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, key, SignInOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	auth := plugin.Auths("fakeco")
	var first, second, user string
	for _, l := range pluginLogins(mustPlugin(t)) {
		switch auth[l.acct.Key]["key"] {
		case "first-key-1111":
			first = l.acct.Key
		case "second-key-2222":
			second, user = l.acct.Key, l.User
		}
	}
	if first == "" || second == "" || user == "" {
		t.Fatalf("initial slots: first=%q second=%q user=%q", first, second, user)
	}
	direct := "direct"
	if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, "replacement-key-3333", SignInOptions{User: user, Proxy: &direct}); err != nil {
		t.Fatal(err)
	}
	auth = plugin.Auths("fakeco")
	if len(auth) != 2 || auth[first]["key"] != "first-key-1111" || auth[second]["key"] != "replacement-key-3333" {
		t.Fatalf("selected key was not replaced in place: %v", auth)
	}
	if user := PluginAccountUser("fakeco", second); user == "" || ProxyOfLogin("fakeco", user) != direct {
		t.Fatalf("replacement proxy not kept for its account: %q", user)
	}
	if _, err := PluginAPIKey(ctx, "fakeco", 0, nil, "new-key-4444", SignInOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := len(plugin.Auths("fakeco")); got != 3 {
		t.Fatalf("ordinary new-account sign-in has %d slots, want 3", got)
	}
}

func TestPluginAPIKeyReloginRejectsConcurrentReplacement(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		unblock()
		server.Close()
	})
	t.Setenv("CAS_AUTH_URL", server.URL)
	source := `export const Plugin = async () => ({
  config: c => { c.provider ??= {}; c.provider.casfixture = {name:"CAS", npm:"@ai-sdk/openai-compatible", api:"https://fixture.invalid", models:{one:{name:"One"}}} },
  auth: { provider:"casfixture", methods:[{type:"api", label:"Token", prompts:[{type:"text", key:"credential", message:"Token"}],
    authorize: async inputs => { await fetch(process.env.CAS_AUTH_URL); return {type:"success", key:inputs.credential} }}] }
})`
	file := filepath.Join(t.TempDir(), "cas.mjs")
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := plugin.Add(ctx, file); err != nil {
		t.Fatal(err)
	}
	slot, err := plugin.Import(ctx, "casfixture", map[string]any{"type": "api", "accountId": "Alice", "key": "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := PluginAPIKey(ctx, "casfixture", 0, map[string]string{"credential": "stale"}, "", SignInOptions{User: "Alice"})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("key authorization did not start")
	}
	replacement, _ := json.Marshal(map[string]any{"type": "api", "accountId": "Alice", "key": "concurrent"})
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		auth[slot] = replacement
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-done; err == nil {
		t.Fatal("stale key authorization replaced a newer login")
	}
	if got := plugin.Auths("casfixture")[slot]["key"]; got != "concurrent" {
		t.Fatalf("newer credential was overwritten: %v", got)
	}
}

// Keyless is generic metadata: neither the provider id nor credential is
// known to the host. Ordinary API methods still reject empty input.
func TestPluginKeylessSingleIdentity(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("FAKE_KEYLESS", "1")
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	ps, err := plugin.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || !ps[0].Single || !ps[0].Methods[0].Keyless || ps[0].Methods[1].Keyless {
		t.Fatalf("keyless metadata lost or leaked to OAuth: %+v", ps)
	}
	for _, credential := range []string{"public-fixture", "renewed-fixture"} {
		if id, err := PluginAPIKey(ctx, "fakeco", 0, map[string]string{"credential": credential}, "", SignInOptions{}); err != nil || id != "fakeco" {
			t.Fatalf("keyless sign-in: %q %v", id, err)
		}
	}
	ps, err = plugin.Providers(ctx)
	if err != nil || len(ps[0].Accounts) != 1 {
		t.Fatalf("single identity duplicated: %+v %v", ps, err)
	}
	if _, err := PluginAPIKey(ctx, "fakeco", 1, nil, "", SignInOptions{}); err == nil {
		t.Fatal("OAuth must not accept an empty API key")
	}
}

// An empty key is taken only by an API method the plugin says is keyless:
// a plain API method, or one of another plugin, still asks for the key.
func TestPluginEmptyKeyOnlyKeyless(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	spec := "@example/opencode-acme-auth"
	os.MkdirAll(settings.Dir(), 0o700)
	if err := os.WriteFile(filepath.Join(settings.Dir(), "plugins.json"), []byte(`{"plugins":[{"spec":"`+spec+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: "acme", Spec: spec, Name: "Acme",
		Methods: []plugin.Method{{Type: "api", Label: "API key"}, {Type: "oauth", Label: "Browser", Keyless: true}}}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	for _, m := range []int{0, 1, 2, -1} {
		if _, err := PluginAPIKey(context.Background(), "acme", m, nil, " ", SignInOptions{}); err == nil || err.Error() != "enter the key" {
			t.Errorf("method %d with no key: %v, want enter the key", m, err)
		}
	}
	if _, err := PluginAPIKey(context.Background(), "nobody", 0, nil, "", SignInOptions{}); err == nil || err.Error() != "enter the key" {
		t.Errorf("an unknown plugin with no key: %v", err)
	}
}

// An API method whose own questions ask its key (keyAsked) is signed in
// with its answers and no key: the window asks none beside them.
func TestPluginEmptyKeyKeyAsked(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	spec := "@example/opencode-acme-auth"
	os.MkdirAll(settings.Dir(), 0o700)
	if err := os.WriteFile(filepath.Join(settings.Dir(), "plugins.json"), []byte(`{"plugins":[{"spec":"`+spec+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: "acme", Spec: spec, Name: "Acme",
		Methods: []plugin.Method{{Type: "api", Label: "Session token", KeyAsked: true}}}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	if _, err := PluginAPIKey(context.Background(), "acme", 0, map[string]string{"token": "t"}, "", SignInOptions{}); err != nil && err.Error() == "enter the key" {
		t.Errorf("a keyAsked method with no key: %v", err)
	}
}
