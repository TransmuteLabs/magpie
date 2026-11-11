package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZCodeActivationFailureKeepsUnrelatedProviderUnstarted(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	counter := filepath.Join(t.TempDir(), "models-count")
	t.Setenv("ZCODE_OTHER_COUNTER", counter)
	source := `export default async () => ({
  config(c) { c.provider ??= {}; c.provider.unrelated = {name:"Unrelated", npm:"@ai-sdk/openai-compatible", models:{one:{name:"One"}}} },
  provider: {id:"unrelated", async models(p) { const f=Bun.file(process.env.ZCODE_OTHER_COUNTER); const n=await f.exists() ? Number(await f.text()) : 0; await Bun.write(f,String(n+1)); return p.models }},
  auth: {provider:"unrelated", methods:[{type:"api",label:"key"}]}
})`
	path := filepath.Join(t.TempDir(), "unrelated.mjs")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, "unrelated", map[string]any{"type": "api", "key": "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Providers(ctx); err != nil {
		t.Fatal(err)
	}
	Refreshed()
	Told()
	before, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	hostMu.Lock()
	previous := current
	hostMu.Unlock()
	if previous == nil {
		t.Fatal("unrelated provider host was not started")
	}
	if err := ensureBundledZCode(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundledTarget(), "index.mjs"), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EditAuth(func(m map[string]json.RawMessage) (bool, error) {
		m["zcode"] = json.RawMessage(`{"type":"oauth","refresh":"fixture","accountId":"zcode"}`)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	ps, err := Providers(ctx)
	if err != nil {
		t.Fatalf("unrelated listing lost: %v", err)
	}
	var other, zcode *Provider
	for i := range ps {
		switch ps[i].ID {
		case "unrelated":
			other = &ps[i]
		case "zcode":
			zcode = &ps[i]
		}
	}
	if other == nil || other.Name != "Unrelated" || zcode == nil || !zcode.FellBack || !strings.Contains(zcode.ListError, "unavailable") {
		t.Fatalf("damaged runtime was hidden or unrelated provider lost: %+v", ps)
	}
	after, err := os.ReadFile(counter)
	if err != nil || string(after) != string(before) {
		t.Fatalf("unrelated models refreshed: before=%s after=%s err=%v", before, after, err)
	}
	hostMu.Lock()
	unchanged := current == previous && current.alive()
	hostMu.Unlock()
	if !unchanged {
		t.Fatal("damaged ZCode restarted or stopped the unrelated host")
	}
	for _, p := range Cached() {
		if p.ID == "zcode" && (!p.FellBack || p.ListError == "") {
			t.Fatalf("cached ZCode hid activation failure: %+v", p)
		}
	}
}

func TestZCodeEmptyProfileDiscoveryDoesNotStartBun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("MAGPIE_BUN", filepath.Join(home, "bun-must-not-run"))
	Settle()
	t.Cleanup(Settle)
	ps, err := Providers(context.Background())
	if err != nil || Running() {
		t.Fatalf("empty discovery started a host: %+v, %v", ps, err)
	}
	if len(ps) != 1 || ps[0].ID != "zcode" || ps[0].Spec != bundledZCodePackage {
		t.Fatalf("intrinsic discovery = %+v", ps)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("empty discovery touched registry storage: %v", err)
	}
}

func TestZCodeFetchRejectsPartialOrForeignBinding(t *testing.T) {
	for _, req := range []FetchRequest{
		{Provider: "zcode", ZCodeScope: "scope"},
		{Provider: "zcode", ZCodeGeneration: "generation"},
		{Provider: "other", BundleSpec: bundledZCodePackage, BundleTarget: bundledTarget(), ZCodeScope: "scope", ZCodeGeneration: "generation"},
		{Provider: "zcode", BundleSpec: "external", BundleTarget: bundledTarget(), ZCodeScope: "scope", ZCodeGeneration: "generation"},
	} {
		if response, err := Fetch(context.Background(), req); err == nil {
			if response != nil {
				response.Body.Close()
			}
			t.Fatalf("invalid intrinsic binding accepted: %+v", req)
		}
	}
}
