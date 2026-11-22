package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/access"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// #2216 (Kenoia): a key held to models or accounts on a shared magpie was
// told no quota at all (#1981), so the remote magpie it was set up on
// showed none of the providers it may use. A key whose owner turns its
// quotas on is told the cards of the providers whose every model it may
// use, of the accounts and keys it may use, and nothing else: not another
// provider's, not a provider's it may use only some models of, not an
// account's it may not use.
func TestHeldKeyToldItsOwnQuotas(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	seedKeptBalances(t)

	ref := func(id, user string) string {
		t.Helper()
		p, err := provider.Find(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range p.AccountIDs() {
			if a.User == user {
				return id + "/" + a.ID
			}
		}
		t.Fatalf("%s has no key %q", id, user)
		return ""
	}
	keys, secrets := newCaller(t, "Owner", "AlphaAll", "AlphaSome", "AlphaTwo", "BetaKey", "Budget", "Nothing")
	set := func(action string, c access.Change) {
		t.Helper()
		if _, err := access.Update(action, c); err != nil {
			t.Fatal(action, err)
		}
	}
	set("models-key", access.Change{Key: keys[1].ID, Models: []string{"alpha/*"}})
	set("models-key", access.Change{Key: keys[2].ID, Models: []string{"alpha/m1", "beta/*"}})
	set("models-key", access.Change{Key: keys[3].ID, Models: []string{"alpha/*"}})
	set("accounts-key", access.Change{Key: keys[3].ID, Accounts: []string{ref("alpha", "Two")}})
	set("accounts-key", access.Change{Key: keys[4].ID, Accounts: []string{ref("beta", provider.Mask("beta-key"))}})
	set("limit-key", access.Change{Key: keys[5].ID, Limit: &access.Limit{Period: "day", Tokens: 1_000_000}})
	set("models-key", access.Change{Key: keys[6].ID, Models: []string{"alpha/m2"}})
	for _, k := range append(keys[1:5:5], keys[6]) {
		set("quotas-key", access.Change{Key: k.ID, Quotas: true})
	}

	h := lanGuard(New().Handler())
	do := func(method, path, secret string) (int, string) {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "192.168.1.9:5000"
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	told := func(path, b string) []string {
		t.Helper()
		var out struct {
			Data []struct{ Provider, User string }
		}
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatal(path, err, b)
		}
		var cards []string
		for _, q := range out.Data {
			if q.Provider == "alpha" || q.Provider == "beta" || q.Provider == "" {
				cards = append(cards, q.Provider+":"+strings.ToLower(q.User))
				continue
			}
			// another provider's card (an account another test signed in)
			cards = append(cards, q.Provider+":?")
		}
		slices.Sort(cards)
		return cards
	}
	every := []string{"alpha:one", "alpha:two", "beta:"}
	want := map[string][]string{
		"AlphaAll":  {"alpha:one", "alpha:two"},
		"AlphaSome": {"beta:"}, // none of alpha's: m2 spends its quota too
		"AlphaTwo":  {"alpha:two"},
		// beta's key alone and no model: beta's models alone are the
		// key's (Linka on Discord), so alpha's cards are not its
		"BetaKey": {"beta:"},
	}
	for _, path := range []string{"/v1/magpie/quotas?kept=1", provider.RemoteCardsPath} {
		for i, s := range secrets {
			name := keys[i].Name
			c, b := do("GET", path, s)
			switch name {
			case "Owner":
				if c != 200 {
					t.Fatalf("%s %s: %d %s", name, path, c, b)
				}
				got := told(path, b)
				for _, w := range every {
					if !slices.Contains(got, w) {
						t.Fatalf("%s %s: no %s card in %v", name, path, w, got)
					}
				}
			case "Nothing":
				// told no card here: the other magpie shows its own limit,
				// not "nothing read on that magpie yet"
				if path == provider.RemoteCardsPath {
					if c != http.StatusForbidden || !strings.Contains(b, provider.KeyHeldCode) {
						t.Errorf("%s %s: %d %s", name, path, c, b)
					}
				} else if c != 200 || len(told(path, b)) != 0 {
					t.Errorf("%s %s: %d %s", name, path, c, b)
				}
			case "Budget":
				if c != http.StatusForbidden || !strings.Contains(b, provider.KeyHeldCode) {
					t.Errorf("%s %s, quotas off: %d %s", name, path, c, b)
				}
			default:
				if c != 200 {
					t.Fatalf("%s %s: %d %s", name, path, c, b)
				}
				if got := told(path, b); !slices.Equal(got, want[name]) {
					t.Errorf("%s %s: told %v, want %v", name, path, got, want[name])
				}
			}
		}
	}
	// the history and the report read afresh go by the same cards
	for _, path := range []string{"/v1/magpie/quotas", "/v1/magpie/quotas/history"} {
		for i, s := range secrets[1:5] {
			name := keys[i+1].Name
			c, b := do("GET", path, s)
			if c != 200 {
				t.Fatalf("%s %s: %d %s", name, path, c, b)
			}
			for _, card := range told(path, b) {
				if !slices.Contains(want[name], card) {
					t.Errorf("%s %s: told %s, may be told only %v", name, path, card, want[name])
				}
			}
		}
	}
	// the accounts list names the same accounts and keys, and nothing else
	for i, s := range secrets[1:5] {
		name := keys[i+1].Name
		c, b := do("GET", AccountsPath, s)
		if c != 200 {
			t.Fatalf("%s accounts: %d %s", name, c, b)
		}
		var out struct{ Data []listedAccount }
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatal(err, b)
		}
		var got []string
		for _, a := range out.Data {
			switch a.Provider {
			case "alpha":
				got = append(got, "alpha:"+strings.ToLower(a.Label))
			case "beta":
				got = append(got, "beta:")
			default:
				got = append(got, a.Provider+":?")
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want[name]) {
			t.Errorf("%s accounts: told %v, want %v", name, got, want[name])
		}
	}
	if c, b := do("GET", AccountsPath, secrets[5]); c != http.StatusForbidden || !strings.Contains(b, provider.KeyHeldCode) {
		t.Errorf("Budget accounts, quotas off: %d %s", c, b)
	}
	// a card the key isn't told isn't read again for it
	for _, x := range []struct{ secret, card string }{
		{secrets[3], "provider=alpha&user=One"},
		{secrets[3], "provider=beta"},
		{secrets[2], "provider=alpha&user=Two"},
		{secrets[1], "provider=beta"},
	} {
		if c, b := do("POST", provider.RemoteRefreshPath+"?"+x.card, x.secret); c != http.StatusForbidden || !strings.Contains(b, provider.KeyHeldCode) {
			t.Errorf("refresh %s: %d %s", x.card, c, b)
		}
	}
	c, b := do("POST", provider.RemoteRefreshPath+"?provider=alpha&user=Two", secrets[3])
	if c != 200 {
		t.Fatalf("refresh its own card: %d %s", c, b)
	}
	// read again, a key with no balance to read has no card: only what
	// it is told may be left
	for _, card := range told("refresh", b) {
		if !slices.Contains(want["AlphaTwo"], card) {
			t.Errorf("refresh its own card: told %s", card)
		}
	}
	c, b = do("POST", provider.RemoteRefreshPath, secrets[1])
	if c != 200 {
		t.Fatalf("refresh every card it is told: %d %s", c, b)
	}
	for _, card := range told("refresh", b) {
		if !slices.Contains(want["AlphaAll"], card) {
			t.Errorf("refresh every card it is told: told %s", card)
		}
	}
}

func TestQuotaSurfacesColdStoreHonorDeadline(t *testing.T) {
	for _, scenario := range []struct {
		name, method, path string
		held               bool
	}{
		{"cards", http.MethodGet, provider.RemoteCardsPath, true},
		{"refresh-all", http.MethodPost, provider.RemoteRefreshPath, true},
		{"refresh-card", http.MethodPost, provider.RemoteRefreshPath + "?provider=alpha&user=Two", true},
		{"history", http.MethodGet, "/v1/magpie/quotas/history", true},
		{"live", http.MethodGet, "/v1/magpie/quotas", false},
		{"kept", http.MethodGet, "/v1/magpie/quotas?kept=1", false},
		{"statusline", http.MethodGet, StatuslinePath + "?format=json", false},
		{"accounts", http.MethodGet, AccountsPath, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			t.Setenv("MAGPIE_ADDR", "")
			seedKeptBalances(t)
			manager := extensionhost.Default()
			source, manifest := t.TempDir(), extensionGatewayManifest()
			manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.AccountAuth)
			if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), mustJSON(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("catalog metadata fixture"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := extensionhost.NewCatalog(manager.Namespace()).Install(t.Context(), source,
				func(context.Context, extensionhost.Installed) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: manifest.ID, Provider: manifest.Providers[0].ID,
				ID: "test", Auth: json.RawMessage(`{"access":"fixture"}`)}, 0); err != nil {
				t.Fatal(err)
			}
			keys, secrets := newCaller(t, "Alpha Two")
			for action, change := range map[string]access.Change{
				"models-key":   {Key: keys[0].ID, Models: []string{"alpha/*"}},
				"accounts-key": {Key: keys[0].ID, Accounts: []string{"alpha/" + provider.KeyID("alpha-two")}},
				"quotas-key":   {Key: keys[0].ID, Quotas: true},
			} {
				if _, err := access.Update(action, change); err != nil {
					t.Fatal(action, err)
				}
			}
			handler := lanGuard(New().Handler())
			call := func(ctx context.Context, method, path string, held bool) *httptest.ResponseRecorder {
				request := httptest.NewRequestWithContext(ctx, method, path, nil)
				request.RemoteAddr = "127.0.0.1:5000"
				if held {
					request.RemoteAddr = "192.168.1.9:5000"
					request.Header.Set("Authorization", "Bearer "+secrets[0])
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			provider.Changed()
			unlock, held := filelock.Lock(filepath.Join(manager.Namespace(), "private", ".lock"), time.Second)
			if !held {
				t.Fatal("could not hold the current SDK Store")
			}
			synctest.Test(t, func(t *testing.T) {
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					done <- call(ctx, scenario.method, scenario.path, scenario.held)
				}()
				synctest.Wait()
				time.Sleep(2 * time.Second)
				synctest.Wait()
				select {
				case response := <-done:
					unlock()
					if response.Body.Len() != 0 {
						t.Errorf("an expired quota request published a response: %s", response.Body)
					}
				default:
					unlock()
					<-done
					t.Error("quota request still blocked on cold discovery after its deadline")
				}
			})
			remoteRefreshes.Lock()
			attempts := len(remoteRefreshes.at)
			remoteRefreshes.Unlock()
			if attempts != 0 {
				t.Errorf("an expired quota request started a refresh: %d", attempts)
			}
			response := call(t.Context(), http.MethodGet, provider.RemoteCardsPath, true)
			var result struct {
				Data []struct{ Provider, User string }
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
				len(result.Data) != 1 || result.Data[0].Provider != "alpha" || result.Data[0].User != "Two" {
				t.Fatalf("the held key lost its exact account scope after discovery canceled: %d %s", response.Code, response.Body)
			}
		})
	}
}

// seedKeptBalances saves provider alpha with two keys, One and Two (models
// m1 and m2), and beta with one key (o1), each key's balance kept on disk
// (quotas.json) as last read.
func seedKeptBalances(t *testing.T, firstWindows ...provider.QuotaWindow) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "alpha", Name: "Alpha", Key: "alpha-one", KeyName: "One",
		Keys: []provider.KeyAccount{{Name: "Two", Key: "alpha-two"}}, Chat: up.URL + "/v1", Models: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "beta", Name: "Beta", Key: "beta-key", Chat: up.URL + "/v1", Models: []string{"o1"}}); err != nil {
		t.Fatal(err)
	}
	// each key's balance as last read, kept on disk (quotas.json)
	tag := func(key string) string {
		sum := sha256.Sum256([]byte(key))
		return "balance " + hex.EncodeToString(sum[:8])
	}
	at := time.Now().Add(-time.Minute)
	kept := map[string]any{}
	for _, k := range []struct{ id, key, user string }{{"alpha", "alpha-one", "One"}, {"alpha", "alpha-two", "Two"}, {"beta", "beta-key", ""}} {
		card := provider.SubscriptionQuota{Provider: k.id, User: k.user, Balance: "$5.00", Windows: []provider.QuotaWindow{}}
		if k.key == "alpha-one" {
			card.Windows = append(card.Windows, firstWindows...)
		}
		kept[k.id+"/"+tag(k.key)] = map[string]any{"at": at, "quota": card}
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(provider.Path()), "quotas.json"), mustJSON(kept), 0o600); err != nil {
		t.Fatal(err)
	}
	provider.ForgetKeptCardsForTest()
	provider.ForgetBalances()
	provider.ForgetPlanQuotas()

}

// #2216 from the other side: a magpie with that magpie as its provider,
// through a key let see its quotas, shows the cards of what the key may
// use beside the key's own; a key told none is answered as held, so it
// shows its own card and the remote's saying why (yi_time), not "nothing
// read on that magpie yet", which a refresh wouldn't change.
func TestRemoteMagpieShowsHeldKeyItsQuotas(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	seedKeptBalances(t)
	keys, secrets := newCaller(t, "AlphaTwo", "Nothing")
	p, err := provider.Find("alpha")
	if err != nil {
		t.Fatal(err)
	}
	var two string
	for _, a := range p.AccountIDs() {
		if a.User == "Two" {
			two = "alpha/" + a.ID
		}
	}
	for _, c := range []struct {
		action string
		in     access.Change
	}{
		{"models-key", access.Change{Key: keys[0].ID, Models: []string{"alpha/*"}}},
		{"accounts-key", access.Change{Key: keys[0].ID, Accounts: []string{two}}},
		{"models-key", access.Change{Key: keys[1].ID, Models: []string{"alpha/m2"}}},
		{"quotas-key", access.Change{Key: keys[0].ID, Quotas: true}},
		{"quotas-key", access.Change{Key: keys[1].ID, Quotas: true}},
	} {
		if _, err := access.Update(c.action, c.in); err != nil {
			t.Fatal(c.action, err)
		}
	}
	gw := httptest.NewServer(lanGuard(New().Handler()))
	defer gw.Close()
	ctx := t.Context()
	shown := func(secret string) []string {
		t.Helper()
		if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: secret, Chat: gw.URL}); err != nil {
			t.Fatal(err)
		}
		provider.ForgetRemoteCardsForTest()
		var out []string
		for _, q := range provider.RemoteCards(ctx) {
			switch {
			case q.Provider == "office/~keys":
				out = append(out, q.Provider+":"+q.Error) // the key's own
			case strings.HasPrefix(q.Provider, "office"):
				out = append(out, q.Provider+":"+strings.ToLower(q.User)+":"+q.Error)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := shown(secrets[0]); !slices.Equal(got, []string{"office/alpha:two:", "office/~keys:"}) {
		t.Errorf("a key held to alpha's Two: %v", got)
	}
	if got := shown(secrets[1]); len(got) != 2 || got[0] != "office/~keys:" ||
		!strings.HasPrefix(got[1], "office::remote magpie tells this key none of its quotas") {
		t.Errorf("a key told no card: %v", got)
	}
}

// #2333: a window's whole is reckoned from every call routed through the
// account, so a held key told the card isn't told it, while this machine's
// owner (no scope) is; the caller's windows are left as they are.
func TestHeldKeyToldNoWindowHolds(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	windows := []provider.QuotaWindow{
		{Name: "5 hours", Used: 40, Holds: &provider.WindowHolds{Tokens: 900000, Priced: true}},
		{Name: "7 days", Used: 2, HoldsWhy: provider.HoldsLow},
	}
	seedKeptBalances(t, windows...)
	previous := provider.ReckonHolds
	t.Cleanup(func() { provider.ReckonHolds = previous })
	reads := 0
	provider.ReckonHolds = func(_ context.Context, cards []provider.SubscriptionQuota, _ time.Time) []provider.SubscriptionQuota {
		reads++
		return cards
	}
	keys, secrets := newCaller(t, "Owner", "Alpha")
	if _, err := access.Update("models-key", access.Change{Key: keys[1].ID, Models: []string{"alpha/*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("quotas-key", access.Change{Key: keys[1].ID, Quotas: true}); err != nil {
		t.Fatal(err)
	}
	handler := lanGuard(New().Handler())
	readWindows := func(secret string) []provider.QuotaSpan {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/magpie/quotas?kept=1", nil)
		r.RemoteAddr = "192.168.1.9:5000"
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var response struct{ Data []provider.Quota }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("quota report: %d %s", w.Code, w.Body.String())
		}
		for _, card := range response.Data {
			if card.Provider == "alpha" && card.User == "One" && len(card.Windows) == len(windows) {
				return card.Windows
			}
		}
		t.Fatalf("the caller wasn't told the card and its windows: %+v", response.Data)
		return nil
	}
	if got := readWindows(secrets[0]); got[0].Holds == nil || got[1].HoldsWhy != provider.HoldsLow {
		t.Fatalf("the owner wasn't told the whole: %+v", got)
	}
	for index, w := range readWindows(secrets[1]) {
		if w.Name != windows[index].Name || w.Used != windows[index].Used || w.Holds != nil || w.HoldsWhy != "" {
			t.Errorf("a held key should see only the allowance window: %+v", w)
		}
	}
	if reads != 1 {
		t.Errorf("Usage history read %d times, want once for the owner", reads)
	}
	if windows[0].Holds == nil || windows[1].HoldsWhy != provider.HoldsLow {
		t.Errorf("the source windows were written to: %+v", windows)
	}
}
