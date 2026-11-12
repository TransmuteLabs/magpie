package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// A plugin whose models hook can't reach its vendor and gives its
// defaults back keeps the list it told last, for the provider and each
// account, as a built-in whose fetch fails keeps the one it fetched last;
// the vendor back, its list is told again. So too for a hook that throws
// (Cursor's, Grok's, Devin's) rather than give its defaults back, and for
// one handing back a table of its own that it says is a fallback.
func TestFallenBackListKeepsTheLastOne(t *testing.T) {
	for _, throws := range []string{"", "1", "own"} {
		t.Run("throws="+throws, func(t *testing.T) { fallenBack(t, throws) })
	}
}

func TestZCodeAuthoritativeEmptySurvivesPersistenceAndFailedListing(t *testing.T) {
	sandbox(t)
	last := []Provider{{ID: "zcode", Spec: "builtin:zcode", Models: []Model{{ID: "GLM-5.3-Flash"}},
		Accounts: []Account{{Key: "zcode", Models: []string{"GLM-5.3-Flash"}}}}}
	empty := []Provider{{ID: "zcode", Spec: "builtin:zcode", Models: []Model{}, ModelsKnownEmpty: true,
		Accounts: []Account{{Key: "zcode", Models: []string{}}}}}
	got := keepListed(empty, last)
	if len(got[0].Models) != 0 || !got[0].ModelsKnownEmpty || got[0].Accounts[0].Models == nil {
		t.Fatalf("a successful empty list was revived: %+v", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var disk []Provider
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	if !disk[0].ModelsKnownEmpty || disk[0].Accounts[0].Models == nil || len(disk[0].Accounts[0].Models) != 0 {
		t.Fatalf("empty account authority lost on disk: %s", b)
	}
	failed := []Provider{{ID: "zcode", Spec: "builtin:zcode", FellBack: true, ListError: "offline",
		Models: []Model{{ID: "GLM-5.3"}}, Accounts: []Account{{Key: "zcode", Models: []string{"GLM-5.3"}, FellBack: true, ListError: "offline"}}}}
	got = keepListed(failed, disk)
	if len(got[0].Models) != 0 || !got[0].ModelsKnownEmpty || got[0].Accounts[0].Models == nil || len(got[0].Accounts[0].Models) != 0 {
		t.Fatalf("failure revived defaults over last-good empty: %+v", got)
	}
}

func TestZCodeFailedAccountKeepsItsEmptyListBesideLiveAccount(t *testing.T) {
	sandbox(t)
	last := []Provider{{ID: "zcode", Spec: "builtin:zcode", Models: []Model{{ID: "live"}},
		Accounts: []Account{{Key: "zcode", Models: []string{}}, {Key: "zcode#2", Models: []string{"live"}}}}}
	now := []Provider{{ID: "zcode", Spec: "builtin:zcode", Models: []Model{{ID: "fallback"}, {ID: "live"}},
		Accounts: []Account{{Key: "zcode", Models: []string{"fallback"}, FellBack: true, ListError: "offline"},
			{Key: "zcode#2", Models: []string{"live"}}}}}
	got := keepListed(now, last)[0]
	if len(got.Models) != 1 || got.Models[0].ID != "live" || got.Accounts[0].Models == nil ||
		len(got.Accounts[0].Models) != 0 || got.Accounts[0].FellBack || !slices.Equal(got.Accounts[1].Models, []string{"live"}) {
		t.Fatalf("unknown account revived defaults beside a live account: %+v", got)
	}
}

func TestZCodeFailedListingKeepsOlderSingleAccountCatalog(t *testing.T) {
	sandbox(t)
	var last []Provider
	if err := json.Unmarshal([]byte(`[{"id":"zcode","spec":"builtin:zcode","models":[{"id":"old-start"}],"accounts":[{"key":"zcode"}]}]`), &last); err != nil {
		t.Fatal(err)
	}
	now := []Provider{{ID: "zcode", Spec: "builtin:zcode", FellBack: true, ListError: "offline",
		Models: []Model{{ID: "default"}}, Accounts: []Account{{Key: "zcode", Models: []string{"default"}, FellBack: true}}}}
	got := keepListed(now, last)[0]
	if len(got.Models) != 1 || got.Models[0].ID != "old-start" || !slices.Equal(got.Accounts[0].Models, []string{"old-start"}) || got.Accounts[0].FellBack {
		t.Fatalf("older single-account list was not retained: %+v", got)
	}
}

func fallenBack(t *testing.T, throws string) {
	sandbox(t)
	t.Setenv("FAKE_MODELS_THROW", throws)
	var down atomic.Bool
	var name atomic.Value
	name.Store("extra")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			// the connection dropped, as a vendor gone away drops it
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		}
		fmt.Fprint(w, name.Load())
	}))
	defer srv.Close()
	t.Setenv("FAKE_MODELS", srv.URL+"/models")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("testdata/fake/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k1", "k2"} {
		if _, err := APIKey(ctx, "fakeco", 0, nil, k, NewAccount); err != nil {
			t.Fatal(err)
		}
	}
	has := func(ps []Provider, id string) bool {
		t.Helper()
		if len(ps) != 1 || len(ps[0].Accounts) != 2 {
			t.Fatalf("Providers = %+v", ps)
		}
		in := slices.ContainsFunc(ps[0].Models, func(m Model) bool { return m.ID == id })
		for _, a := range ps[0].Accounts {
			if slices.Contains(a.Models, id) != in {
				t.Fatalf("account %s's models %v, the provider's having %s: %v", a.Key, a.Models, id, in)
			}
		}
		return in
	}
	ps, err := Providers(ctx)
	if err != nil || !has(ps, "fake-extra") {
		t.Fatalf("with the vendor up, Providers = %+v, %v", ps, err)
	}

	down.Store(true)
	ps, err = RefreshProviderModels(ctx, "fakeco", "fakeco")
	if err == nil || !has(ps, "fake-extra") || ps[0].FellBack {
		t.Fatalf("explicit failed refresh did not report its source while keeping the list: %+v, %v", ps, err)
	}
	ps, err = Providers(ctx)
	if err != nil || !has(ps, "fake-extra") || ps[0].FellBack {
		t.Fatalf("with the vendor down, the list told last went: %+v, %v", ps, err)
	}
	// so after a restart, from the list kept on disk
	Restart()
	provMu.Lock()
	provCache = nil
	provMu.Unlock()
	if ps, err = Providers(ctx); err != nil || !has(ps, "fake-extra") {
		t.Fatalf("after a restart, Providers = %+v, %v", ps, err)
	}

	down.Store(false)
	name.Store("newer")
	if ps, err = Providers(ctx); err != nil || !has(ps, "fake-newer") || has(ps, "fake-extra") {
		t.Fatalf("with the vendor back, Providers = %+v, %v", ps, err)
	}
}
