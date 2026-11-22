package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

// A plugin with auth.refresh leaves renewing its sign-ins to magpie: a
// token close to its end is renewed once however many requests find it
// so, each of them sent with the new one; one with time left, or no
// expiry, isn't renewed; one the vendor turns away is marked expired and
// not tried again, and one that can't be renewed goes with the old token
// and isn't tried again at once, nor is one whose refresh gives nothing
// back while it is due (magpie-ima-auth's, JZGiVvkSOA75376 on X).
func TestPluginRenewsSignIns(t *testing.T) {
	sandbox(t)
	var mu sync.Mutex
	var seen []string
	srv := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	logf := filepath.Join(t.TempDir(), "renewals")
	t.Setenv("RENEW_LOG", logf)
	said := make(chan string, 16)
	OnSignIn(func(provider, account, s string, epoch *string) {
		notice := provider + " " + account + " " + s
		if epoch != nil {
			notice += " " + *epoch
		}
		said <- notice
	})
	t.Cleanup(func() { OnSignIn(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	abs, _ := filepath.Abs("testdata/renew/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(time.Minute).UnixMilli() // inside the 5 minutes
	later := time.Now().Add(time.Hour).UnixMilli()
	account := func(who, refresh string, expires int64) string {
		t.Helper()
		k, err := Import(ctx, "renewco", map[string]any{"type": "oauth", "refresh": refresh, "access": "old-" + who, "expires": expires, "accountId": who})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	due := account("due@renew", "r-ok", soon)
	notYet := account("later@renew", "r-ok-later", later)
	never := account("never@renew", "r-ok-never", 0)
	gone := account("gone@renew", "r-gone", soon)
	down := account("down@renew", "r-down", soon)
	none := account("none@renew", "r-none", time.Now().Add(-time.Minute).UnixMilli())
	send := func(account string) string {
		t.Helper()
		res, err := Fetch(ctx, FetchRequest{Provider: "renewco", Account: account, Model: "renew-1", NPM: "@ai-sdk/openai-compatible",
			URL: srv.URL + "/v1/chat/completions", Method: "POST", Body: []byte(`{}`)})
		if err != nil {
			t.Fatalf("%s: %v", account, err)
		}
		io.ReadAll(res.Body)
		res.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		return seen[len(seen)-1]
	}
	renewals := func() []string {
		b, _ := os.ReadFile(logf)
		return strings.Fields(string(b))
	}
	stored := func(key string) map[string]any {
		var all map[string]map[string]any
		b, _ := os.ReadFile(AuthPath())
		json.Unmarshal(b, &all)
		return all[key]
	}

	// five requests at once on the account due: one renewal, all sent
	// with what it gave, and it is kept
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); send(due) }()
	}
	wg.Wait()
	if got := renewals(); len(got) != 1 || got[0] != "r-ok" {
		t.Fatalf("renewals = %v, want r-ok once", got)
	}
	mu.Lock()
	for _, h := range seen {
		if h != "Bearer a-1" {
			t.Errorf("a request went with %q, not the renewed token", h)
		}
	}
	mu.Unlock()
	if a := stored(due); a["access"] != "a-1" || a["refresh"] != "r-ok+" || a["accountId"] != "due@renew" {
		t.Fatalf("kept %v", a)
	}
	if h := send(due); h != "Bearer a-1" || len(renewals()) != 1 {
		t.Fatalf("renewed again: %q, %v", h, renewals())
	}
	if s := waitSaid(t, said); s != "renewco "+due+" renewed "+savedEpoch(t, ctx, due) {
		t.Fatal("renewal did not report the renewed account's epoch")
	}

	// time left, or no expiry: not renewed
	if h := send(notYet); h != "Bearer old-later@renew" {
		t.Fatalf("an account with time left went with %q", h)
	}
	if h := send(never); h != "Bearer old-never@renew" {
		t.Fatalf("an account with no expiry went with %q", h)
	}
	if got := renewals(); len(got) != 1 {
		t.Fatalf("renewals = %v", got)
	}

	// turned away for good: marked, sent as it is, and not tried again
	if h := send(gone); h != "Bearer old-gone@renew" {
		t.Fatalf("a refused account went with %q", h)
	}
	if s := waitSaid(t, said); s != "renewco "+gone+" expired "+savedEpoch(t, ctx, gone) {
		t.Fatal("refused renewal did not report the observed account's epoch")
	}
	send(gone)
	// can't be reached: sent as it is, not tried again at once
	if h := send(down); h != "Bearer old-down@renew" {
		t.Fatalf("an account that couldn't renew went with %q", h)
	}
	send(down)
	// renewed nothing, its token past its end: sent as it is, and the
	// next request doesn't ask the plugin again
	if h := send(none); h != "Bearer old-none@renew" {
		t.Fatalf("an account its plugin renewed nothing for went with %q", h)
	}
	send(none)
	send(none)
	if got := renewals(); strings.Join(got, " ") != "r-ok r-gone r-down r-none" {
		t.Fatalf("renewals = %v", got)
	}
	if a := stored(gone); a["access"] != "old-gone@renew" {
		t.Fatalf("a refused account was changed: %v", a)
	}
}

func TestFetchLoaderUsesCurrentSavedSignIn(t *testing.T) {
	sandbox(t)
	t.Setenv("RENEW_LOG", filepath.Join(t.TempDir(), "renewals"))
	t.Setenv("RENEW_CAPTURE_AUTH", "1")
	srv := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "Bearer models-token" {
			w.Header().Set("X-Renew-Access", "fetch-token")
		}
		io.WriteString(w, token)
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	abs, err := filepath.Abs("testdata/renew/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	const account = "renewco"
	if err := Restore(ctx, "renewco", account, map[string]any{
		"type": "oauth", "refresh": "r-ok", "access": "old-token", "expires": 1, "accountId": "user@renew",
	}); err != nil {
		t.Fatal(err)
	}
	epochs, err := CurrentAuthEpochs(ctx)
	if err != nil || epochs[account] == "" {
		t.Fatalf("restore had no sign-in epoch: %v", err)
	}
	fetch := func(requested, want string) FetchAuth {
		t.Helper()
		res, auth, err := FetchWithAuth(ctx, FetchRequest{Provider: "renewco", Account: requested, Model: "renew-1",
			NPM: "@ai-sdk/openai-compatible", URL: srv.URL + "/v1/chat/completions", Method: "POST", Body: []byte(`{}`),
			Headers: map[string]string{"X-Fake-Run": os.Getenv("FAKE_RUN")}})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		token, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != http.StatusOK || string(token) != want {
			t.Fatalf("vendor saw %q, want %q (status %d, %v)", token, want, res.StatusCode, err)
		}
		return auth
	}
	assertFetchEpoch(t, fetch("", "Bearer models-token"), account, epochs[account])
	if err := WithAuthEpoch(ctx, account, epochs[account], func() error { return nil }); err != nil {
		t.Fatalf("renewal or client.auth.set replaced the sign-in epoch: %v", err)
	}
	if saved := Auths("renewco")[account]; saved["access"] != "fetch-token" || saved["refresh"] != "r-ok+" {
		t.Fatalf("renewal, models and fetch did not keep their saves: access=%v, refresh=%v", saved["access"], saved["refresh"])
	}
	assertFetchEpoch(t, fetch(account, "Bearer fetch-token"), account, epochs[account])
	if err := EditAuth(func(records map[string]json.RawMessage) (bool, error) {
		records[account] = json.RawMessage(`{"type":"oauth","refresh":"r-replaced","access":"new-token","expires":9000000000000000,"accountId":"user@renew"}`)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	assertFetchEpoch(t, fetch(account, "Bearer new-token"), account, savedEpoch(t, ctx, account))
	legacy := `{"renewco":{"type":"oauth","refresh":"r-legacy","access":"legacy-token","expires":9000000000000000}}`
	if err := os.WriteFile(AuthPath(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	assertFetchEpoch(t, fetch(account, "Bearer legacy-token"), account, "")
	if got := fetch("renewco#absent", ""); got.Account != "renewco#absent" || got.Epoch != nil {
		t.Error("an absent slot had sign-in proof")
	}
}

func waitSaid(t *testing.T, said chan string) string {
	t.Helper()
	select {
	case s := <-said:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("nothing said of the sign-in")
		return ""
	}
}
