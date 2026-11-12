package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

// fakeCodex signs a ChatGPT account in to Codex, its models listed by a
// server of the test's own the way ChatGPT's backend answers. While down is
// there, the list fails, as with no network.
func fakeCodex(t *testing.T, h string) (down string) {
	t.Helper()
	down = filepath.Join(h, "codex-down")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(down); err == nil || r.URL.Path != "/backend-api/codex/models" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var ms []map[string]any
		for i, id := range []string{"gpt-5.5", "gpt-6-luna"} {
			ms = append(ms, map[string]any{"slug": id, "display_name": id, "visibility": "list", "priority": i + 1})
		}
		b, _ := json.Marshal(map[string]any{"models": ms})
		w.Write(b)
	}))
	old := provider.CodexBase
	provider.CodexBase = srv.URL + "/backend-api/codex"
	jwt := func(claims map[string]any) string {
		b, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
	}
	b, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token": jwt(map[string]any{"email": "me@example.com",
			"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro", "chatgpt_account_id": "acct-1"}}),
		"access_token":  jwt(map[string]any{"exp": float64(time.Now().Add(24 * time.Hour).Unix())}),
		"refresh_token": "r", "account_id": "acct-1",
	}})
	write(t, filepath.Join(h, ".codex", "auth.json"), string(b))
	provider.ForgetAccounts()
	t.Cleanup(func() {
		provider.CodexBase = old
		srv.Close()
		provider.ForgetAccounts()
	})
	if p, err := provider.Find("codex"); err != nil || p.Account == nil || !p.Ready() {
		t.Fatalf("the ChatGPT account isn't signed in: %v", err)
	}
	// a cold models.dev sync answered here: no test request leaves this
	// process
	client := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: modelsTransport{}}
	t.Cleanup(func() { http.DefaultClient = client })
	return down
}

// A subscription's list exists only at its vendor: the terminal asks for it
// at start and fills a model picker opened before it came, with no m on the
// Providers page (#1789, reyoung: Routing offered cursor/auto alone, from
// Cursor's built-in, since retired; Codex's ChatGPT account stands in).
func TestTUIFetchesSubscriptionModels(t *testing.T) {
	for _, view := range []string{"routing", "routing with catalog sync", "routing with catalog sync last", "provider", "classifier", "agent"} {
		t.Run(view, func(t *testing.T) {
			fakeCodex(t, home(t))
			m := model{w: 120, h: 40, syncing: strings.Contains(view, "sync")}
			init := m.Init()
			m, want := openModelPicker(t, m, view)
			m = typeIn(m, "gpt-5.5")
			// the list fetched adds codex/gpt-5.5 under this filter: the
			// model on the cursor stays, not its row
			// (Codex's own picker has none before its list: nothing to hold)
			selected, _ := m.pk.choice()
			if selected == "" && view != "provider" {
				t.Fatalf("no GPT-5.5 in the picker\n%s", m.View())
			}
			m, _ = runModelsCmd(m, init, strings.HasSuffix(view, "last"))
			if got, _ := m.pk.choice(); selected != "" && got != selected || m.pk.input.Value() != "gpt-5.5" || m.mode != modePick {
				t.Fatalf("the list arriving changed the open picker: choice %q, filter %q, mode %v", got, m.pk.input.Value(), m.mode)
			}
			m.pk.input.SetValue("gpt-6-luna")
			m.pk.refilter()
			// a row of the list, not the typed text a picker that takes a
			// value of one's own hands back when no row matches
			if got, _ := m.pk.choice(); got != want || len(m.pk.match) == 0 {
				t.Fatalf("the picker hasn't Codex's listed model: got %q, want %q\n%s", got, want, m.View())
			}
		})
	}
}

// A list that couldn't be had at start (no network yet) is asked for again
// while the terminal stays open, not only after a restart.
func TestTUIAsksAgainForAMissingList(t *testing.T) {
	down := fakeCodex(t, home(t))
	write(t, down, "")
	old := modelsRetry
	modelsRetry = 0
	t.Cleanup(func() { modelsRetry = old })

	m, _ := openModelPicker(t, model{w: 120, h: 40}, "routing")
	m, retry := runModelsCmd(m, m.Init(), false)
	if retry == nil {
		t.Fatal("Codex's list failed and nothing asks for it again")
	}
	if err := os.Remove(down); err != nil {
		t.Fatal(err)
	}
	provider.ForgetAccounts() // the minute FetchNew waits after a failure
	m, again := runModelsCmd(m, retry, false)
	m.pk.input.SetValue("gpt-6-luna")
	m.pk.refilter()
	if got, _ := m.pk.choice(); got != "codex/gpt-6-luna" {
		t.Fatalf("asked again, still no gpt-6-luna: %q\n%s", got, m.View())
	}
	if again != nil {
		t.Fatal("asks again with every list there")
	}
}

func TestTUIPluginRefetchReportsSourceFailure(t *testing.T) {
	bun := testenv.Bun(t)
	home(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("FAKE_MODELS_THROW", "1")
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		fmt.Fprint(w, "extra")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_MODELS", srv.URL)
	plugin.Settle()
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec, err := filepath.Abs("../plugin/testdata/fake/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.APIKey(ctx, "fakeco", 0, nil, "fixture", plugin.NewAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "fakeco", Models: []string{"fake-extra"}}); err != nil {
		t.Fatal(err)
	}
	ok := refetchCmd("fakeco")().(flashMsg)
	if !ok.ok || !strings.Contains(ok.text, "models from its list") {
		t.Fatalf("successful plugin refresh: %+v", ok)
	}
	down.Store(true)
	failed := refetchCmd("fakeco")().(flashMsg)
	if failed.ok || strings.Contains(failed.text, "models from its list") || !strings.Contains(failed.text, "couldn't list") {
		t.Fatalf("failed plugin refresh appeared successful: %+v", failed)
	}
	p, err := provider.Find("fakeco")
	if err != nil || len(p.Models) != 1 || p.Models[0] != "fake-extra" {
		t.Fatalf("failed refresh changed saved picks: %+v, %v", p, err)
	}
	if !slices.ContainsFunc(p.Available(), func(m catalog.Model) bool { return m.ID == "fake-extra" }) {
		t.Fatal("failed refresh lost the last known catalog")
	}
}

func TestTUIZCodeRefetchEmptyAndFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated Bun preload wrapper requires a POSIX shell")
	}
	bun := testenv.Bun(t)
	h := home(t)
	var mode atomic.Int32 // 0: known model, 1: authoritative empty, 2: unavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if mode.Load() == 2 {
			http.Error(w, "fixture offline", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/api/v1/client/configs":
			fmt.Fprint(w, `{"code":0,"data":{"configs":{"builtin_provider_config_json":"https://cdn.example/config.json"}}}`)
		case "/config.json":
			fmt.Fprint(w, `{"schemaVersion":1,"revision":9007199254740000,"config":{
			  "providerConfigRules":{"templateRules":[],"providerRules":[{"providerId":"account:zai-start-plan","config":{
			    "group":"zai-family","builtinModelIds":["GLM-5.3-Flash"],
			    "access":{"type":"zhipu-account","accountType":"zai","mode":"start-plan"},
			    "api":{"type":"anthropic-messages","baseUrl":"https://zcode.z.ai/api/v1/zcode-plan/anthropic"}}}]},
			  "modelConfigRules":{"modelRules":[{"modelMatch":".*","config":{"enabled":true}}],
			    "modelApiRules":[],"providerSiteRules":[],"templateModelRules":[],"builtinProviderModelRules":[]}}}`)
		case "/api/v1/zcode-plan/billing/balance":
			now := time.Now().Unix()
			var balances []map[string]any
			if mode.Load() == 0 {
				balances = []map[string]any{{"plan_id": "start", "user_plan_id": "fixture", "capabilities": []string{"model:GLM-5.3-Flash"}, "remaining_units": 10}}
			} else {
				balances = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"server_time": now,
				"plans":    []map[string]any{{"plan_id": "start", "user_plan_id": "fixture", "status": "active", "ends_at": now + 3600}},
				"balances": balances}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	preload, wrapper := filepath.Join(dir, "preload.mjs"), filepath.Join(dir, "bun-wrapper")
	write(t, preload, `const original = globalThis.fetch;
globalThis.fetch = (input, init) => {
  const u = new URL(input instanceof Request ? input.url : String(input));
  return original(process.env.ZCODE_TUI_FIXTURE + u.pathname + u.search, init);
};`)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	write(t, wrapper, fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = run ]; then shift; exec %s run --preload %s \"$@\"; fi\nexec %s \"$@\"\n", quote(bun), quote(preload), quote(bun)))
	if err := os.Chmod(wrapper, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_BUN", wrapper)
	t.Setenv("ZCODE_TUI_FIXTURE", srv.URL)
	local := filepath.Join(h, ".zcode", "v2", "runtime", "provider", "magpie-tui-test", "release", "data", "zcode-builtin.json")
	write(t, local, `{"schemaVersion":1,"revision":9007199254740991,"config":null}`)
	plugin.Settle()
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	state, _ := json.Marshal(map[string]any{"site": "zai", "kind": "start", "uid": "fixture-user",
		"zcodeUserId": "fixture-user", "device": "fixture-device", "jwt": "fixture-jwt"})
	if _, err := plugin.Import(ctx, "zcode", map[string]any{"type": "oauth", "refresh": string(state),
		"access": "fixture-jwt", "accountId": "fixture-user", "uid": "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	plugin.Refreshed()
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "zcode", Models: []string{"GLM-5.3-Flash"}}); err != nil {
		t.Fatal(err)
	}
	got := refetchCmd("zcode")().(flashMsg)
	if !got.ok || !strings.Contains(got.text, "models from its list") {
		t.Fatalf("initial ZCode refresh = %+v", got)
	}
	mode.Store(2)
	got = refetchCmd("zcode")().(flashMsg)
	if got.ok || strings.Contains(got.text, "models from its list") {
		t.Fatalf("failed ZCode refresh appeared authoritative: %+v", got)
	}
	p, err := provider.Find("zcode")
	if err != nil || !slices.Contains(p.Models, "GLM-5.3-Flash") {
		t.Fatalf("failed refresh lost the saved pick: %+v, %v", p, err)
	}
	mode.Store(1)
	got = refetchCmd("zcode")().(flashMsg)
	if got.ok || !strings.Contains(got.text, "0 models from its list") || !strings.Contains(got.text, "GLM-5.3-Flash") {
		t.Fatalf("authoritative empty refresh did not report dropped picks: %+v", got)
	}
	p, err = provider.Find("zcode")
	if err != nil || slices.Contains(p.Models, "GLM-5.3-Flash") || !slices.Contains(p.Delisted, "GLM-5.3-Flash") {
		t.Fatalf("empty refresh did not set the pick aside: %+v, %v", p, err)
	}
}

// openModelPicker opens one of the terminal's four pickers of models, and
// says what Codex's gpt-6-luna is called in it.
func openModelPicker(t *testing.T, m model, view string) (model, string) {
	t.Helper()
	switch view {
	case "provider":
		m.page = pageProviders
		m.reloadProviders()
		for i, p := range m.provs {
			if p.ID == "codex" {
				m.prow = i
			}
		}
		m.openProviderModels("codex")
		return m, "gpt-6-luna"
	case "classifier":
		m.page = pageGroups
		m.openClassifier(provider.Group{ID: "test", Name: "Test"})
	case "agent":
		a, err := agent.Find("codex")
		if err != nil {
			t.Fatal(err)
		}
		m.agents = []*agent.Agent{a}
		m.reload()
		m.openFieldPicker()
		// Codex has no list of its own here (no models_cache.json), so the
		// models magpie serves on its ChatGPT account are offered via magpie
		// (codexViaMagpieFor)
		return m, "codex/gpt-6-luna"
	default:
		m.page = pageGroups
		m.openMemberPicker(pickMsg{newGroup: "Test"})
	}
	return m, "codex/gpt-6-luna"
}

// runModelsCmd runs what Init or a retry returned and hands its messages to
// the model, as the program would, held until the picker is open to stand
// for a vendor answering late; reverse delivers a batch's last first. It
// returns the command the models' message left, the retry, if any.
func runModelsCmd(m model, cmd tea.Cmd, reverse bool) (model, tea.Cmd) {
	if cmd == nil {
		return m, nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var left tea.Cmd
		for i := range batch {
			j := i
			if reverse {
				j = len(batch) - 1 - i
			}
			var c tea.Cmd
			if m, c = runModelsCmd(m, batch[j], reverse); c != nil {
				left = c
			}
		}
		return m, left
	}
	next, left := m.Update(msg)
	return next.(model), left
}

type modelsTransport struct{}

func (modelsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() == "127.0.0.1" {
		return http.DefaultTransport.RoundTrip(r) // the test's own vendor
	}
	if r.URL.String() != "https://models.dev/api.json" {
		return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"openai":{"id":"openai","models":{}}}`)), Request: r}, nil
}
