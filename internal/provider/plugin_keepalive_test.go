package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/testenv"
)

// fakeSignIns replaces what the keeper sees and does: the sign-ins kept
// alive, and the host's answer for each, counting what it was asked.
func fakeSignIns(t *testing.T, ins *[]pluginSignIn, answer func(pluginSignIn) (plugin.KeptSignedIn, error)) map[string]int {
	t.Helper()
	asked := map[string]int{}
	wasIns, wasKeep := pluginSignInsKept, keepPluginSignedIn
	t.Cleanup(func() { pluginSignInsKept, keepPluginSignedIn = wasIns, wasKeep })
	pluginSignInsKept = func(context.Context) []pluginSignIn { return *ins }
	keepPluginSignedIn = func(_ context.Context, s pluginSignIn) (plugin.KeptSignedIn, error) {
		asked[s.Key]++
		return answer(s)
	}
	return asked
}

// The keeper asks the host of a sign-in only once it is due, and sleeps
// until the next comes due (no longer than keepAliveLookMost, no shorter
// than keepAliveLookLeast): an idle magpie with nothing due asks nothing
// (#5080).
func TestKeeperAsksOnlyWhatIsDue(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ins := []pluginSignIn{
		{ID: "orcaterm", Key: "orcaterm:a", At: now.Add(-time.Second)},
		{ID: "orcaterm", Key: "orcaterm:b", At: now.Add(90 * time.Second)},
		{ID: "kiro", Key: "kiro:c", At: now.Add(time.Hour)},
	}
	asked := fakeSignIns(t, &ins, func(pluginSignIn) (plugin.KeptSignedIn, error) {
		return plugin.KeptSignedIn{Due: true, Renewed: true}, nil
	})
	var k signInKeeper
	if d := k.look(context.Background(), now); d != 90*time.Second {
		t.Fatalf("next look in %v, want 90s (b comes due)", d)
	}
	if asked["orcaterm:a"] != 1 || asked["orcaterm:b"] != 0 || asked["kiro:c"] != 0 {
		t.Fatalf("asked %v, want a only", asked)
	}
	if len(k.held) != 0 {
		t.Fatalf("a renewed sign-in is held: %v", k.held)
	}
	ins = ins[2:]
	if d := k.look(context.Background(), now); d != keepAliveLookMost {
		t.Fatalf("nothing due for an hour: next look in %v, want %v", d, keepAliveLookMost)
	}
	ins = nil
	if d := k.look(context.Background(), now); d != keepAliveLookMost || len(asked) != 1 {
		t.Fatalf("no sign-ins: next look in %v, asked %v", d, asked)
	}
}

// A renewal that fails is tried again later and later, for that sign-in
// only, while the others are renewed as they come due; once it is
// renewed (or the sign-in made again, with another due time) it starts
// afresh. A renewal that is due, but the host found waiting on an earlier
// failure, is held as one.
func TestKeeperBacksOffAFailedRenewal(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	ins := []pluginSignIn{{ID: "orcaterm", Key: "orcaterm:a", At: due}, {ID: "orcaterm", Key: "orcaterm:b", At: due}}
	down := true
	asked := fakeSignIns(t, &ins, func(s pluginSignIn) (plugin.KeptSignedIn, error) {
		if s.Key == "orcaterm:a" && down {
			return plugin.KeptSignedIn{Due: true, Err: "OrcaTerm can't be reached"}, nil
		}
		return plugin.KeptSignedIn{Due: true, Renewed: true}, nil
	})
	var k signInKeeper
	ctx := context.Background()
	at := now
	look := func(want time.Duration) {
		t.Helper()
		if d := k.look(ctx, at); d != want {
			t.Fatalf("at +%v: next look in %v, want %v", at.Sub(now), d, want)
		}
	}
	look(time.Minute) // a failed once: a minute
	if asked["orcaterm:a"] != 1 || asked["orcaterm:b"] != 1 {
		t.Fatalf("asked %v", asked)
	}
	ins[1].At = now.Add(time.Hour) // b renewed
	at = now.Add(30 * time.Second)
	look(30 * time.Second) // held: not asked
	if asked["orcaterm:a"] != 1 {
		t.Fatalf("asked again inside its backoff: %v", asked)
	}
	at = now.Add(time.Minute)
	look(2 * time.Minute) // failed twice: two minutes
	at = at.Add(2 * time.Minute)
	look(4 * time.Minute) // three times: four
	if asked["orcaterm:a"] != 3 || asked["orcaterm:b"] != 1 {
		t.Fatalf("asked %v", asked)
	}
	for range 10 {
		at = at.Add(30 * time.Minute)
		k.look(ctx, at)
	}
	if f := k.held[signInDue{"orcaterm:a", due.UnixMilli()}]; f.until.Sub(at) != 30*time.Minute {
		t.Fatalf("held %v after many failures, want at most 30m", f.until.Sub(at))
	}

	// signed in again: another due time, and it starts afresh
	ins[0].At = at.Add(-time.Second)
	look(time.Minute)
	if len(k.held) != 1 {
		t.Fatalf("the old due time's hold was kept: %v", k.held)
	}

	// the vendor is back
	down = false
	at = at.Add(time.Minute)
	n := asked["orcaterm:a"]
	k.look(ctx, at)
	if asked["orcaterm:a"] != n+1 || len(k.held) != 0 {
		t.Fatalf("renewed, still held: %v, asked %v", k.held, asked)
	}

	// an error of the call, or due and not renewed with no word why, is
	// a failure too
	ins = ins[:1]
	ins[0].At = due
	fails := []plugin.KeptSignedIn{{Due: true}, {Due: true, Waiting: true}}
	for _, f := range fails {
		keepPluginSignedIn = func(context.Context, pluginSignIn) (plugin.KeptSignedIn, error) { return f, nil }
		k = signInKeeper{}
		if d := k.look(ctx, now); d != time.Minute {
			t.Fatalf("%+v: next look in %v, want held a minute", f, d)
		}
	}
	keepPluginSignedIn = func(context.Context, pluginSignIn) (plugin.KeptSignedIn, error) {
		return plugin.KeptSignedIn{}, errNoBun
	}
	k = signInKeeper{}
	if d := k.look(ctx, now); d != time.Minute {
		t.Fatalf("no Bun: next look in %v, want held a minute", d)
	}
}

// A sign-in the host finds not due (it reckons the lead otherwise) is
// held keepAliveLookMost, not asked every look; and a look made as magpie
// stops records nothing of the sign-in it was renewing.
func TestKeeperHoldsWhatTheHostFindsNotDue(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ins := []pluginSignIn{{ID: "kiro", Key: "kiro:a", At: now.Add(-time.Second)}}
	asked := fakeSignIns(t, &ins, func(pluginSignIn) (plugin.KeptSignedIn, error) {
		return plugin.KeptSignedIn{}, nil
	})
	var k signInKeeper
	if d := k.look(context.Background(), now); d != keepAliveLookMost {
		t.Fatalf("next look in %v, want %v", d, keepAliveLookMost)
	}
	k.look(context.Background(), now.Add(time.Minute))
	k.look(context.Background(), now.Add(2*time.Minute))
	if asked["kiro:a"] != 1 {
		t.Fatalf("asked %d times inside the hold", asked["kiro:a"])
	}
	k.look(context.Background(), now.Add(keepAliveLookMost))
	if asked["kiro:a"] != 2 {
		t.Fatalf("asked %d times after the hold", asked["kiro:a"])
	}

	ctx, cancel := context.WithCancel(context.Background())
	keepPluginSignedIn = func(c context.Context, s pluginSignIn) (plugin.KeptSignedIn, error) {
		cancel()
		<-c.Done()
		return plugin.KeptSignedIn{}, c.Err()
	}
	k = signInKeeper{}
	k.look(ctx, now)
	if len(k.held) != 0 {
		t.Fatalf("a renewal cut short by magpie stopping was held: %v", k.held)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal(ctx.Err())
	}
}

func TestKeepAliveBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 5: 16 * time.Minute, 6: 30 * time.Minute, 50: 30 * time.Minute} {
		if got := keepAliveBackoff(n); got != want {
			t.Errorf("keepAliveBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestKeepPluginsSignedInCancelsDiscovery(t *testing.T) {
	for _, boundary := range []string{"owner", "logins"} {
		t.Run(boundary, func(t *testing.T) {
			claudeHome(t)
			was := plugin.ListInBackground(false)
			t.Cleanup(func() { plugin.ListInBackground(was) })
			t.Cleanup(plugin.Settle)
			wasFirst := keepAliveLookFirst
			keepAliveLookFirst = 0
			t.Cleanup(func() { keepAliveLookFirst = wasFirst })

			const spec = "renewal-fixture"
			writeFile(t, plugin.ListPath(), plugin.List{Plugins: []plugin.Entry{{Spec: spec}}})
			plugin.UseCached([]plugin.Provider{{Spec: spec, ID: "renewco", Renews: true}})
			writeFile(t, plugin.AuthPath(), map[string]any{"renewco": map[string]any{
				"type": "oauth", "expires": time.Now().Add(time.Hour).UnixMilli(), "accountId": "new@renew",
			}})
			saveLogins(t, savedLogin{Agent: "plugin:renewco", Home: "renewco", User: "old@renew", On: true})
			before, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}

			namespace := filepath.Join(filepath.Dir(plugin.AuthPath()), "extensions")
			root, lock := namespace, filepath.Join("private", ".lock")
			reached := pluginsBuilt.Load
			if boundary == "logins" {
				root, lock = filepath.Dir(loginsMu.Path()), filepath.Base(loginsMu.Path())
				reached = pluginLoginLocks.Load
			}
			initial := reached()
			err = whileExtensionAccountLockHeld(t, root, lock, func(ctx context.Context) error {
				KeepPluginsSignedIn(ctx)
				return ctx.Err()
			})
			if !errors.Is(err, context.DeadlineExceeded) || reached() <= initial {
				t.Fatalf("canceled discovery did not reach %s: %v", boundary, err)
			}
			if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("canceled discovery changed logins: %v", err)
			}
			if plugin.Running() {
				t.Fatal("discovery started the plugin host")
			}
		})
	}
}

func TestPluginRenewalCommitsSignInWithoutListing(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("RENEW_LOG", filepath.Join(t.TempDir(), "renewals"))
	t.Setenv("RENEW_CAPTURE_AUTH", "1")
	was := plugin.ListInBackground(false)
	t.Cleanup(func() { plugin.ListInBackground(was) })
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	spec, err := filepath.Abs("../plugin/testdata/renew/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, spec); err != nil {
		t.Fatal(err)
	}
	key, err := plugin.Import(ctx, "renewco", map[string]any{
		"type": "oauth", "refresh": "r-ok", "access": "old",
		"expires": time.Now().Add(time.Minute).UnixMilli(), "accountId": "me@renew",
	})
	if err != nil {
		t.Fatal(err)
	}
	saveLogins(t, savedLogin{
		Agent: "plugin:renewco", Home: key, User: "me@renew", On: true, Lapsed: "sign in again", Plan: "Existing plan",
	})
	plugin.UseCached(nil)
	listing := filepath.Join(filepath.Dir(plugin.AuthPath()), "plugin-providers.json")
	if err := os.Remove(listing); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	unlock, held := filelock.Lock(loginsMu.Path(), time.Second)
	if !held {
		t.Fatal("could not hold the login lock")
	}
	defer func() { unlock() }()
	renewal, err := plugin.KeepSignedIn(ctx, "renewco", key)
	cancel()
	if err != nil || !renewal.Renewed || plugin.Auths("renewco")[key]["access"] != "a-1" {
		t.Fatalf("renewal did not save credentials: %v (%+v)", err, renewal)
	}
	unlock()
	unlock = func() {}
	plugin.Told()

	login := loginOf(t, readLogins(), "plugin:renewco", "me@renew")
	if login.Lapsed != "" || login.Plan != "Existing plan" || login.PluginObservations.SignIn.Revision == 0 {
		t.Fatalf("renewal did not complete the saved sign-in: %+v", login)
	}
	if _, known := plugin.Listed(); known {
		t.Fatal("a saved renewal fetched a new provider listing")
	}
	if plugin.Auths("renewco")[key]["access"] != "a-1" {
		t.Fatal("a model listing overwrote the saved renewal")
	}
}

// Only the sign-ins switched on and not marked, of a plugin provider not
// switched off, are kept alive: an account switched off, one the vendor
// turned away and every account of a provider switched off are left as
// they are (#5080).
func TestKeptSignInsAreTheOnesSwitchedOn(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("RENEW_LOG", filepath.Join(t.TempDir(), "renewals"))
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/renew/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(time.Minute).UnixMilli()
	keys := map[string]string{}
	for _, who := range []string{"on@renew", "off@renew", "lapsed@renew"} {
		k, err := plugin.Import(ctx, "renewco", map[string]any{"type": "oauth", "refresh": "r-ok-" + who, "access": "old", "expires": soon, "accountId": who})
		if err != nil {
			t.Fatal(err)
		}
		keys[who] = k
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	plugin.Refreshed()
	kept := func() map[string]bool {
		t.Helper()
		got := map[string]bool{}
		for _, s := range pluginSignInsKept(ctx) {
			if s.ID != "renewco" || s.At.UnixMilli() != soon-5*60*1000 {
				t.Fatalf("kept %+v", s)
			}
			got[s.Key] = true
		}
		return got
	}
	if got := kept(); len(got) != 3 {
		t.Fatalf("kept %v, want all three", got)
	}
	var pp plugin.Provider
	for _, p := range heldPlugins() {
		if p.ID == "renewco" {
			pp = p
		}
	}
	users := map[string]string{}
	for _, l := range pluginLogins(pp) {
		users[l.acct.Key] = l.User
	}
	if err := setPluginLoginOn(pp, users[keys["off@renew"]], false); err != nil {
		t.Fatal(err)
	}
	notePluginLapse(ctx, pp, fixtureFetchAuth(t, ctx, keys["lapsed@renew"]), http.StatusUnauthorized, false)
	if got := kept(); len(got) != 1 || !got[keys["on@renew"]] {
		t.Fatalf("kept %v, want on@renew alone", got)
	}
	if err := SetOff(PluginID("renewco"), true); err != nil {
		t.Fatal(err)
	}
	if got := kept(); len(got) != 0 {
		t.Fatalf("a provider switched off: kept %v", got)
	}
}
