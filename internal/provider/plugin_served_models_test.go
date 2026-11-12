package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

// A plugin provider offers the models any account switched on serves,
// not the first account's alone: a model only a later account lists is
// in the provider's catalog, and an account switched off adds nothing
// (#2554, willywang; LoKi saw it again on 0.1.1320). Each account keeps
// its own list for routing.
func TestPluginProviderOffersEveryOnAccountsModels(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	spec := "@example/opencode-acme-auth"
	write := func(name string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		os.MkdirAll(settings.Dir(), 0o700)
		if err := os.WriteFile(filepath.Join(settings.Dir(), name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("plugins.json", map[string]any{"plugins": []map[string]any{{"spec": spec}}})
	write("plugin-auth.json", map[string]map[string]any{
		"acme":   {"type": "api", "key": "sk-acme-0001"},
		"acme#2": {"type": "api", "key": "sk-acme-0002"},
		"acme#3": {"type": "api", "key": "sk-acme-0003"},
	})
	plugin.UseCached([]plugin.Provider{{ID: "acme", Spec: spec, Name: "Acme",
		Methods: []plugin.Method{{Type: "api", Label: "API key"}},
		Accounts: []plugin.Account{
			{Key: "acme", Type: "api", Models: []string{"a"}},
			{Key: "acme#2", Type: "api", Models: []string{"a", "b"}},
			{Key: "acme#3", Type: "api", Models: []string{"c"}},
		},
		Models: listedModels(t, `[{"id": "a", "name": "A"}, {"id": "b", "name": "B"}, {"id": "c", "name": "C"}]`),
	}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	saveLogins(t,
		savedLogin{Agent: "plugin:acme", User: "one", Home: "acme", On: true},
		savedLogin{Agent: "plugin:acme", User: "two", Home: "acme#2", On: true},
		savedLogin{Agent: "plugin:acme", User: "three", Home: "acme#3"},
	)
	ids := func(p Provider) string {
		var out []string
		for _, m := range p.Available() {
			out = append(out, m.ID)
		}
		return strings.Join(out, " ")
	}
	ps := pluginAccounts()
	i := slices.IndexFunc(ps, func(p Provider) bool { return p.ID == "acme" })
	if i < 0 {
		t.Fatalf("no acme provider in %v", ps)
	}
	if got := ids(ps[i]); got != "a b" {
		t.Fatalf("the provider offers %q, want %q", got, "a b")
	}
	if !ps[i].Account.Lists("a") || ps[i].Account.Lists("b") {
		t.Fatal("the first account routes past its own list")
	}
	per := map[string]string{}
	for _, l := range pluginLogins(mustCached(t, "acme")) {
		per[l.acct.Key] = ids(pluginProvider(mustCached(t, "acme"), l))
	}
	if per["acme"] != "a" || per["acme#2"] != "a b" || per["acme#3"] != "c" {
		t.Fatalf("each account's own models: %v", per)
	}
}

func TestPluginEmptyAccountDoesNotRouteAnotherAccountsModels(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{"zcode": map[string]any{"type": "api", "key": "fixture-a"},
		"zcode#2": map[string]any{"type": "api", "key": "fixture-b"}}
	encoded, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(settings.Dir(), "plugin-auth.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	pp := plugin.Provider{ID: "zcode", Spec: "builtin:zcode", Models: []plugin.Model{{ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash"}},
		Accounts: []plugin.Account{{Key: "zcode", Models: []string{}}, {Key: "zcode#2", Models: []string{"GLM-5.3-Flash"}}}}
	b, err := json.Marshal(pp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pp); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{pp})
	t.Cleanup(func() { plugin.UseCached(nil) })
	if got := pluginAccountCatalog(pp, "zcode"); len(got) != 0 {
		t.Fatalf("empty account received another account's models: %+v", got)
	}
	if got := pluginServedCatalog(pp, []string{"zcode"}); len(got) != 0 {
		t.Fatalf("empty account union = %+v", got)
	}
	if got := pluginServedCatalog(pp, []string{"zcode", "zcode#2"}); len(got) != 1 || got[0].ID != "GLM-5.3-Flash" {
		t.Fatalf("live account disappeared from union: %+v", got)
	}
	a := &Account{plugin: &pp, pluginKey: "zcode"}
	if a.pluginLists("GLM-5.3-Flash") {
		t.Fatal("empty account can route another account's model")
	}
	a.pluginKey = "zcode#2"
	if !a.pluginLists("GLM-5.3-Flash") {
		t.Fatal("live account cannot route its model")
	}
}

// The actual embedded hook and host read a source-shaped local vendor. The
// CDN revision in the URL changes independently of the cached client config.
func TestZCodeCatalogRefreshAndEmptyThroughHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bun preload wrapper requires a POSIX shell")
	}
	bun := testenv.Bun(t)
	home := claudeHome(t)
	dir := t.TempDir()
	control := filepath.Join(dir, "control.json")
	preload := filepath.Join(dir, "preload.mjs")
	wrapper := filepath.Join(dir, "bun-wrapper")
	localDir := filepath.Join(home, ".zcode", "v2", "runtime", "provider", "magpie-catalog-test", "release", "data")
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	localFile := filepath.Join(localDir, "zcode-builtin.json")
	writeLocal := func(body string) {
		t.Helper()
		if err := os.WriteFile(localFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const unavailableLocal = `{"schemaVersion":1,"revision":9007199254740991,"config":null}`
	writeLocal(unavailableLocal)
	source := `
const file = Bun.file(process.env.ZCODE_CATALOG_FIXTURE)
globalThis.fetch = async (input) => {
  const state = await file.json()
  const u = new URL(input instanceof Request ? input.url : String(input))
  if (state.down) throw new Error("fixture offline")
  if (state.configDown && (u.pathname === "/api/v1/client/configs" || u.hostname === "cdn.example"))
    throw new Error("fixture config offline")
  const ok = data => Response.json({code: 0, data})
  if (u.pathname === "/api/v1/client/configs")
    return ok({configs: {builtin_provider_config_json: "https://cdn.example/config-" + state.revision + ".json"}})
  if (u.hostname === "cdn.example") {
    const revision = Number(u.pathname.match(/config-(\d+)\.json$/)?.[1])
    const id = "account:zai-start-plan", model = "GLM-5.3-Flash"
    return Response.json({schemaVersion: 1, revision: 9007199254740000 + revision, config: {
      providerConfigRules: {templateRules: [], providerRules: [{providerId: id, config: {
        group: "zai-family", builtinModelIds: [model],
        access: {type: "zhipu-account", accountType: "zai", mode: "start-plan"},
        api: {type: "anthropic-messages", baseUrl: "https://zcode.z.ai/api/v1/zcode-plan/anthropic"},
      }}]},
      modelConfigRules: {
        modelRules: [{modelMatch: ".*", config: {enabled: true, properties: {
          contextWindow: 1000000, inputFormat: {supportsImage: false, supportsVideo: false, supportsPdf: false, supportsAudio: false}},
          optionSpecs: {maxOutputTokens: {max: 128000}, reasoningLevel: {values: ["low", "high"]}}}}],
        modelApiRules: [],
        providerSiteRules: [{modelMatch: ".*", apiTypeMatch: "anthropic-messages",
          baseUrlMatch: "https://zcode\\.z\\.ai/api/v1/zcode-plan/anthropic/?",
          config: {properties: {inputFormat: {supportsImage: revision >= 2}}}}],
        templateModelRules: [],
        builtinProviderModelRules: [{providerId: id, modelId: model, config: {enabled: revision !== 3}}],
      },
    }})
  }
  if (u.pathname === "/api/v1/zcode-plan/billing/balance") {
    const now = Math.floor(Date.now() / 1000)
    return ok({server_time: now, plans: [{plan_id: "start", user_plan_id: "fixture", status: "active", ends_at: now + 3600}],
      balances: state.empty ? [] : state.models.map(model =>
        ({plan_id: "start", user_plan_id: "fixture", capabilities: ["model:" + model], remaining_units: 10}))})
  }
  return new Response("unexpected fixture request", {status: 404})
}
`
	if err := os.WriteFile(preload, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = run ]; then shift; exec %s run --preload %s \"$@\"; fi\nexec %s \"$@\"\n", quote(bun), quote(preload), quote(bun))
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_BUN", wrapper)
	t.Setenv("ZCODE_CATALOG_FIXTURE", control)
	t.Cleanup(plugin.Settle)
	configDown := false
	modelNames := []string{"GLM-5.3-Flash"}
	set := func(revision int, empty, down bool) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"revision": revision, "empty": empty, "down": down, "configDown": configDown, "models": modelNames})
		if err := os.WriteFile(control, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set(1, false, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	state := map[string]any{"site": "zai", "kind": "start", "uid": "fixture-user", "zcodeUserId": "fixture-user",
		"device": "fixture-device", "jwt": "fixture-jwt"}
	auth := func(jwt string) map[string]any {
		state["jwt"] = jwt
		b, _ := json.Marshal(state)
		return map[string]any{"type": "oauth", "refresh": string(b), "access": jwt, "accountId": "fixture-user", "uid": "fixture-user"}
	}
	account, err := plugin.Import(ctx, "zcode", auth("fixture-jwt"))
	if err != nil {
		t.Fatal(err)
	}
	plugin.Refreshed()
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	current := func() plugin.Provider {
		t.Helper()
		pp, ok := PluginOf("zcode")
		if !ok {
			t.Fatal("embedded ZCode provider not listed")
		}
		return pp
	}
	image := func() bool {
		t.Helper()
		pp := current()
		for _, m := range pp.Models {
			if m.ID == "GLM-5.3-Flash" {
				return m.Image && m.ImageSaid
			}
		}
		t.Fatalf("Start model missing: %+v", pp)
		return false
	}
	if image() {
		t.Fatal("revision 1 did not disable image input")
	}
	set(2, false, false)
	if _, err := plugin.Providers(ctx); err != nil || image() {
		t.Fatalf("ordinary discovery unexpectedly bypassed the positive cache: %v", err)
	}
	p, err := Find("zcode")
	if err != nil || p.Account == nil {
		t.Fatalf("finding Start provider: %+v, %v", p, err)
	}
	if _, err := p.Fetch(ctx); err != nil || !image() {
		t.Fatalf("explicit refresh did not reach the new client and CDN revision: %v", err)
	}
	set(3, false, false)
	if ms, err := p.Fetch(ctx); err != nil || len(ms) != 0 {
		t.Fatalf("all-disabled revision = %+v, %v", ms, err)
	}
	if pp := current(); !pp.ModelsKnownEmpty || len(pp.Models) != 0 || pp.Accounts[0].Models == nil {
		t.Fatalf("disabled models returned through the host: %+v", pp)
	}
	set(3, false, true)
	if _, err := p.Fetch(ctx); err == nil {
		t.Fatal("failed explicit refresh appeared successful")
	}
	if pp := current(); !pp.ModelsKnownEmpty || len(pp.Models) != 0 || len(pp.Accounts[0].Models) != 0 {
		t.Fatalf("failed refresh revived the disabled list: %+v", pp)
	}
	set(4, false, false)
	if ms, err := p.Fetch(ctx); err != nil || len(ms) == 0 || !image() {
		t.Fatalf("recovery did not refresh the catalog: %+v, %v", ms, err)
	}
	writeLocal(`{"schemaVersion":1,"revision":9007199254740991,"config":{
	  "providerConfigRules":{"templateRules":[],"providerRules":[{"providerId":"account:zai-start-plan","config":{
	    "group":"zai-family","builtinModelIds":["GLM-5.3-Flash"],
	    "api":{"type":"anthropic-messages","baseUrl":"https://zcode.z.ai/api/v1/zcode-plan/anthropic"},
	    "access":{"type":"zhipu-account","mode":"start-plan","accountType":"zai"}}}]},
	  "modelConfigRules":{"modelRules":[{"modelMatch":".*","config":{"enabled":true}}],
	    "modelApiRules":[],"providerSiteRules":[],"templateModelRules":[],
	    "builtinProviderModelRules":[{"providerId":"account:zai-start-plan","modelId":"GLM-5.3-Flash","config":{"enabled":false}}]}}}`)
	configDown = true
	set(4, false, false)
	if _, err := p.Fetch(ctx); err == nil {
		t.Fatal("remote config failure was hidden by the installed config")
	}
	if pp := current(); !pp.ModelsKnownEmpty || pp.FellBack || len(pp.Models) != 0 || len(pp.Accounts[0].Models) != 0 {
		t.Fatalf("valid installed all-disabled config revived the previous model: %+v", pp)
	}
	writeLocal(unavailableLocal)
	configDown = false
	modelNames = []string{"GLM-5.3", "GLM-5.3-Flash"}
	set(4, false, false)
	if _, err := p.Fetch(ctx); err != nil || !p.Account.Lists("GLM-5.3") || !p.Account.Lists("GLM-5.3-Flash") {
		t.Fatalf("both Start models were not published: %v", err)
	}
	if err := Save(Provider{ID: "zcode", Models: []string{"GLM-5.3", "GLM-5.3-Flash"}}); err != nil {
		t.Fatal(err)
	}
	configDown = true
	modelNames = []string{"GLM-5.3"}
	set(4, false, false)
	if _, err := p.Fetch(ctx); err == nil {
		t.Fatal("unavailable config beside a fresh nonempty balance appeared successful")
	}
	if pp := current(); pp.FellBack || p.Account.Lists("GLM-5.3-Flash") || !p.Account.Lists("GLM-5.3") {
		t.Fatalf("fresh balance did not remove the old Start model: %+v", pp)
	}
	after, err := Find("zcode")
	if err != nil || slices.Contains(after.Models, "GLM-5.3-Flash") || !slices.Contains(after.Delisted, "GLM-5.3-Flash") {
		t.Fatalf("removed Start model stayed among saved picks: %+v, %v", after, err)
	}
	for _, model := range after.Exposed() {
		if model.ID == "GLM-5.3-Flash" {
			t.Fatalf("removed Start model stayed exposed: %+v", model)
		}
	}
	configDown = false
	modelNames = []string{"GLM-5.3-Flash"}
	set(4, false, false)
	if _, err := p.Fetch(ctx); err != nil {
		t.Fatalf("Start catalog did not recover: %v", err)
	}
	set(4, true, false)
	if ms, err := p.Fetch(ctx); err != nil || len(ms) != 0 {
		t.Fatalf("successful empty balance = %+v, %v", ms, err)
	}
	if pp := current(); !pp.ModelsKnownEmpty || len(pp.Models) != 0 || len(pp.Accounts[0].Models) != 0 {
		t.Fatalf("empty Start balance revived defaults: %+v", pp)
	}
	configDown = true
	set(4, true, false)
	if _, err := p.Fetch(ctx); err == nil {
		t.Fatal("unavailable config beside an empty balance appeared to refresh successfully")
	}
	if pp := current(); !pp.ModelsKnownEmpty || len(pp.Models) != 0 {
		t.Fatalf("config failure replaced an authoritative empty balance: %+v", pp)
	}
	if p.Account.Lists("GLM-5.3-Flash") {
		t.Fatal("empty Start account still routes the old model")
	}
	expired := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Unix()-60))) + ".sig"
	if err := plugin.Restore(ctx, "zcode", account, auth(expired)); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	if pp := current(); !pp.ModelsKnownEmpty || len(pp.Models) != 0 {
		t.Fatalf("expired Start account exposed another catalog: %+v", pp)
	}
	usage, err := plugin.AccountUsage(ctx, "zcode", account)
	if err != nil || usage.SignIn != "expired" || !strings.Contains(usage.Error, "sign in") {
		t.Fatalf("expired Start usage = %+v, %v", usage, err)
	}
	user := PluginAccountUser("zcode", account)
	card := pluginLoginQuota(ctx, Login{Agent: "plugin:zcode", User: user})
	if !strings.Contains(card.Error, "sign in") {
		t.Fatalf("expired usage card = %+v", card)
	}
	lapsed := false
	for _, login := range pluginLogins(current()) {
		if login.acct.Key == account && login.Lapsed != "" {
			lapsed = true
		}
	}
	if !lapsed {
		t.Fatal("Start relogin signal did not reach the account lapse state")
	}
}

func mustCached(t *testing.T, id string) plugin.Provider {
	t.Helper()
	pp, ok := PluginOf(id)
	if !ok {
		t.Fatalf("no plugin %s", id)
	}
	return pp
}
