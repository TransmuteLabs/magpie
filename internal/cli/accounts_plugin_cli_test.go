package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	fixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

// magpie accounts lists, switches and forgets a plugin's accounts by its
// provider's id, as a built-in subscription's.
func TestAccountsOfAPlugin(t *testing.T) {
	bun := testenv.Bun(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
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
	for _, key := range []string{"sk-one-key-1111", "sk-two-key-2222"} {
		if _, err := provider.PluginAPIKey(ctx, "fakeco", 0, nil, key, provider.SignInOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	list := func() []accountRow {
		t.Helper()
		out, err := said(t, func() error { return accountsCmd([]string{"accounts", "fakeco", "--json"}) })
		if err != nil {
			t.Fatal(err)
		}
		var rows []accountRow
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return rows
	}
	rows := list()
	if len(rows) != 2 || !rows[0].Active || rows[1].Active || rows[0].User != "API key …1111" || rows[1].User != "API key …2222" {
		t.Fatalf("magpie accounts fakeco = %+v", rows)
	}
	second := rows[1].User
	if _, err := said(t, func() error { return accountsCmd([]string{"accounts", "switch", "fakeco", second}) }); err != nil {
		t.Fatal(err)
	}
	if rows := list(); len(rows) != 2 || rows[0].User != second || !rows[0].Active {
		t.Fatalf("after switching to %s: %+v", second, rows)
	}
	// forgetting the one in use puts the other first, as a built-in's does
	first := rows[0].User
	if _, err := said(t, func() error { return accountsCmd([]string{"accounts", "forget", "fakeco", second}) }); err != nil {
		t.Fatal(err)
	}
	if rows := list(); len(rows) != 1 || rows[0].User != first || !rows[0].Active {
		t.Fatalf("after forgetting %s: %+v", second, rows)
	}
	if _, err := said(t, func() error { return accountsCmd([]string{"accounts", "switch", "nosuch", "a"}) }); err == nil || !strings.Contains(err.Error(), "can be added and switched") {
		t.Fatalf("an unknown one: %v", err)
	}
}

func TestAccountsJSONShowsRealPartialZCodeReading(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	fixture.Start(t)
	var partial atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": v}) }
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			reply(map[string]any{"accessToken": "project-token", "tokenType": "Bearer", "expiresIn": 3600, "expiresAt": time.Now().Unix() + 3600})
		case strings.HasSuffix(r.URL.Path, "/quota/limit") && !partial.Load():
			reply(map[string]any{"limits": []any{map[string]any{"type": "TOKENS_LIMIT", "unit": 3, "number": 5, "percentage": 25}}})
		case strings.HasSuffix(r.URL.Path, "/quota/limit"):
			http.Error(w, "paid unavailable", http.StatusServiceUnavailable)
		case r.URL.Path == "/api/v1/zcode-plan/billing/balance" && partial.Load():
			now := time.Now().Unix()
			reply(map[string]any{"server_time": now, "plans": []any{map[string]any{"plan_id": "start", "user_plan_id": "u", "name": "Start", "status": "active", "ends_at": now + 3600}},
				"balances": []any{map[string]any{"plan_id": "start", "user_plan_id": "u", "show_name": "GLM-5.3", "capabilities": []string{"model:GLM-5.3"}, "total_units": 100, "remaining_units": 60}}})
		case r.URL.Path == "/api/v1/mcp/usage" && partial.Load():
			reply(map[string]any{"server_time": time.Now().Unix(), "total_usage": map[string]any{"used": 4, "limit": 20, "remaining": 16}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("ZCODE_FIXTURE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture.Install(t, ctx)
	list := func() []accountRow {
		t.Helper()
		out, err := said(t, func() error { return accountsCmd([]string{"accounts", "zcode", "--json"}) })
		if err != nil {
			t.Fatal(err)
		}
		var rows []accountRow
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("account JSON: %s: %v", out, err)
		}
		return rows
	}
	first := list()
	if len(first) != 1 || first[0].Error != "" || len(first[0].Windows) != 1 || first[0].Windows[0].Used != 25 {
		t.Fatalf("initial account: %+v", first)
	}
	partial.Store(true)
	provider.StaleAllowance("plugin:zcode", first[0].User)
	after := list()
	if len(after) != 1 || !after[0].Partial || after[0].Error != "Coding Plan quota unavailable" || after[0].ReadAt == nil ||
		len(after[0].Windows) != 1 || after[0].Windows[0].Used != 40 || len(after[0].Observations) != 2 {
		t.Fatalf("partial account JSON: %+v", after)
	}
	expiredAuth := fixture.AuthFor("expired-user", "", "fixture-zcode")
	var state map[string]any
	if err := json.Unmarshal([]byte(expiredAuth["refresh"].(string)), &state); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
	state["oauthToken"] = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	raw, _ := json.Marshal(state)
	expiredAuth["refresh"] = string(raw)
	if _, err := plugin.Import(ctx, "zcode", expiredAuth); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	_ = list() // the actual usage read records the expired sign-in
	rows := list()
	var found bool
	for _, r := range rows {
		if strings.Contains(r.Error, "sign-in has expired") {
			found = r.Lapsed != "" && r.Partial && r.ReadAt != nil && len(r.Windows) > 0
		}
	}
	if !found {
		t.Fatalf("expired account JSON lost its lapse or Start subset: %+v", rows)
	}
}

func TestStatuslineCommandShowsPartialAndStaleRemoteValues(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	old, passed := now.Add(-4*time.Hour), now.Add(-time.Hour)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/magpie/quotas" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []provider.Quota{
			{Provider: "zcode", Kind: "subscription", Error: "paid failed", Partial: true, Windows: []provider.QuotaSpan{{Name: "Start", Used: 40}}},
			{Provider: "office/claude", Kind: "subscription", AsOf: &old, ReadDeferred: true,
				Windows: []provider.QuotaSpan{{Name: "5 hours", Used: 82, ResetsAt: &passed}}},
		}})
	}))
	t.Cleanup(s.Close)
	line := statusline(strings.NewReader(""), nil, s.URL, now)
	for _, word := range []string{"partial", "paid failed", "Start 40%", "as of", "not refreshed", "5h 82%"} {
		if !strings.Contains(line, word) {
			t.Errorf("missing %q: %s", word, line)
		}
	}
	if strings.Contains(line, "5h 0%") {
		t.Fatalf("stale reset became zero: %s", line)
	}
	if only := statusline(strings.NewReader(""), []string{"zcode"}, s.URL, now); strings.Contains(only, "office/claude") || !strings.Contains(only, "paid failed") {
		t.Fatalf("filtered CLI statusline: %s", only)
	}
}
