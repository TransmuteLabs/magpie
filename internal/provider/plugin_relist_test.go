package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// zenFree is the opencode-zen-free plugin's provider listing ids.
func zenFree(fellBack bool, ids ...string) []plugin.Provider {
	p := plugin.Provider{ID: "opencode-zen-free", Spec: "@magpie-community/opencode-zen-free-auth", Name: "OpenCode Zen Free",
		NPM: "@ai-sdk/openai-compatible", FellBack: fellBack}
	for _, id := range ids {
		p.Models = append(p.Models, plugin.Model{ID: id, Name: id, NPM: "@ai-sdk/openai-compatible"})
	}
	return []plugin.Provider{p}
}

// A magpie left running asks the plugins for their lists again, by
// itself: a free model Zen dropped leaves the list and the user's picks,
// a new one is listed, a pick typed in by hand stays, and a list that
// couldn't be read changes nothing (Jeremy.Zhou on Discord).
func TestPluginsListedAgainWhileRunning(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	write := func(name string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(settings.Dir(), name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("plugins.json", map[string]any{"plugins": []map[string]any{{"spec": "@magpie-community/opencode-zen-free-auth"}}})
	write("plugin-auth.json", map[string]any{"opencode-zen-free": map[string]any{"type": "api", "key": "public"}})
	plugin.UseCached(zenFree(false, "big-pickle", "glm-5-free"))
	t.Cleanup(func() { plugin.UseCached(nil) })
	if err := Save(Provider{ID: "opencode-zen-free", Models: []string{"big-pickle", "glm-5-free", "mine-typed"}}); err != nil {
		t.Fatal(err)
	}

	answers := make(chan []plugin.Provider)
	asked := make(chan struct{})
	oldList, oldFirst, oldEvery, oldSoon := relistPlugins, relistFirst, relistEvery, relistSoon
	// asked says the loop asks again, so all it did with the last answer
	// is done
	relistPlugins = func(ctx context.Context) ([]plugin.Provider, error) {
		select {
		case asked <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case ps := <-answers:
			if !ps[0].FellBack {
				plugin.UseCached(ps) // as plugin.Providers keeps it
			}
			return ps, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	relistFirst, relistEvery, relistSoon = time.Millisecond, time.Millisecond, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
		relistPlugins, relistFirst, relistEvery, relistSoon = oldList, oldFirst, oldEvery, oldSoon
	})
	go func() { defer close(done); KeepPluginsListed(ctx) }()
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the plugins weren't asked")
	}

	answer := func(ps []plugin.Provider) {
		t.Helper()
		for _, step := range []func() bool{
			func() bool { answers <- ps; return true },
			func() bool { <-asked; return true },
		} {
			ok := make(chan bool, 1)
			go func() { ok <- step() }()
			select {
			case <-ok:
			case <-time.After(10 * time.Second):
				t.Fatal("the plugins weren't asked again")
			}
		}
	}
	picks := func() []string {
		t.Helper()
		p, err := Find("opencode-zen-free")
		if err != nil {
			t.Fatal(err)
		}
		return p.Models
	}
	available := func() []string {
		t.Helper()
		p, err := Find("opencode-zen-free")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, m := range p.Available() {
			ids = append(ids, m.ID)
		}
		return ids
	}

	// the vendor's list couldn't be read: the plugin told the one before
	answer(zenFree(true, "big-pickle"))
	if got := picks(); !slices.Equal(got, []string{"big-pickle", "glm-5-free", "mine-typed"}) {
		t.Fatalf("a list that fell back dropped picks: %v", got)
	}

	answer(zenFree(false, "big-pickle", "late-free"))
	if got := picks(); !slices.Equal(got, []string{"big-pickle", "mine-typed"}) {
		t.Fatalf("picks after the list was read again: %v", got)
	}
	if got := available(); !slices.Contains(got, "late-free") || slices.Contains(got, "glm-5-free") {
		t.Fatalf("models after the list was read again: %v", got)
	}
}

func TestZCodeAuthoritativeEmptyReconcilesPicksButUnknownDoesNot(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]any{"zcode": map[string]any{"type": "api", "key": "fixture"}})
	if err := os.WriteFile(filepath.Join(settings.Dir(), "plugin-auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	before := []plugin.Provider{{ID: "zcode", Spec: "builtin:zcode",
		Models: []plugin.Model{{ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash"}}}}
	plugin.UseCached(before)
	t.Cleanup(func() { plugin.UseCached(nil) })
	if err := Save(Provider{ID: "zcode", Models: []string{"GLM-5.3-Flash"}}); err != nil {
		t.Fatal(err)
	}
	unknown := []plugin.Provider{{ID: "zcode", Spec: "builtin:zcode", FellBack: true}}
	if gone := dropGonePluginPicks(before, unknown); len(gone) != 0 {
		t.Fatalf("unknown list dropped picks: %v", gone)
	}
	if kept := keptListed(before, unknown); len(kept[0].Models) != 1 {
		t.Fatalf("unknown list lost its baseline: %+v", kept)
	}
	empty := []plugin.Provider{{ID: "zcode", Spec: "builtin:zcode", Models: []plugin.Model{}, ModelsKnownEmpty: true}}
	if gone := dropGonePluginPicks(before, empty)["zcode"]; !slices.Equal(gone, []string{"GLM-5.3-Flash"}) {
		t.Fatalf("authoritative empty list did not delist the old pick: %v", gone)
	}
	if kept := keptListed(before, empty); len(kept[0].Models) != 0 || !kept[0].ModelsKnownEmpty {
		t.Fatalf("empty list was replaced by the old baseline: %+v", kept)
	}
	p, err := Find("zcode")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.Models, "GLM-5.3-Flash") || !slices.Contains(p.Delisted, "GLM-5.3-Flash") {
		t.Fatalf("empty list did not set aside the old pick: models=%v delisted=%v", p.Models, p.Delisted)
	}
	if gone := dropGonePluginPicks(empty, before); len(gone) != 0 {
		t.Fatalf("restored list incorrectly dropped picks: %v", gone)
	}
	p, err = Find("zcode")
	if err != nil || !slices.Contains(p.Models, "GLM-5.3-Flash") || len(p.Delisted) != 0 {
		t.Fatalf("returning model did not restore the pick: %+v, %v", p, err)
	}
}
