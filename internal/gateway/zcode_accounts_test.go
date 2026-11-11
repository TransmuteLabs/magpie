package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/plugin"
	zcodefixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
	"github.com/yetone/magpie/internal/provider"
)

func TestZCodeAccountRoutesRequireFullKeyAndNeverExposeInternalOperations(t *testing.T) {
	codexSignedIn(t)
	t.Setenv("MAGPIE_ADDR", "")
	keys, secrets := newCaller(t, "Full", "Read only", "Held")
	if _, err := access.Update("access-key", access.Change{Key: keys[1].ID, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("limit-key", access.Change{Key: keys[2].ID, Limit: &access.Limit{Period: "day", Tokens: 1000}}); err != nil {
		t.Fatal(err)
	}
	h := lanGuard(New().Handler())
	do := func(path, secret, body string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
		r.Header.Set("Content-Type", "application/json")
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	for _, path := range []string{provider.RemoteZCodeResetPath, provider.RemoteZCodeResetPath + "/scoped", provider.RemoteZCodeAccountPath, provider.RemoteZCodeAccountPath + "/scoped"} {
		body := `{"provider":"zcode","account":"me","window":"week","operation":"context","params":{}}`
		for _, secret := range []string{"", secrets[1], secrets[2]} {
			if code, got := do(path, secret, body); code < 400 || code == http.StatusNotFound {
				t.Errorf("path=%s code=%d body=%s", path, code, got)
			}
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
		r.Header.Set("Origin", "https://attacker.invalid")
		r.Header.Set("Authorization", "Bearer "+secrets[0])
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 || w.Code == http.StatusNotFound {
			t.Errorf("browser origin reached %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{provider.RemoteZCodeResetPath + "/scoped", provider.RemoteZCodeAccountPath + "/scoped"} {
		body := `{"provider":"zcode","account":"me","window":"week","operation":"context"}`
		if code, got := do(path, secrets[0], body); code != http.StatusBadRequest || !strings.Contains(got, "scope") {
			t.Errorf("missing strict expectation on %s: %d %s", path, code, got)
		}
	}
	if code, got := do(provider.RemoteZCodeAccountPath, secrets[0], `{"provider":"zcode","operation":"context"}`); code != http.StatusBadRequest {
		t.Fatalf("missing named account: %d %s", code, got)
	}
	if code, got := do(provider.RemoteZCodeAccountPath, secrets[0], `{"provider":"other/zcode","account":"me","operation":"context"}`); code != http.StatusBadRequest {
		t.Fatalf("chained remote account: %d %s", code, got)
	}
	for _, operation := range []string{"mcp.headers", "reset.use", "reset.opportunity", "offpeak.ticket", "offpeak.settle", "highspeed.draw"} {
		body := fmt.Sprintf(`{"provider":"zcode","account":"me","operation":%q,"params":{},"scope":"scope","generation":"generation"}`, operation)
		code, got := do(provider.RemoteZCodeAccountPath, secrets[0], body)
		if code < 400 || code == http.StatusNotFound {
			t.Errorf("operation=%s code=%d body=%s", operation, code, got)
		}
	}
}

func TestZCodeContextSelectorPinsAdmittedSecondAccount(t *testing.T) {
	fresh(t)
	vendor := zcodefixture.Start(t)
	vendor.SetNativeSuccessAuth("fixture-zcode-b")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, first := zcodefixture.Install(t, ctx)
	second, err := plugin.Import(ctx, "zcode", zcodefixture.AuthFor("北冥", "fixture-business-b", "fixture-zcode-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	pid := provider.PluginID("zcode")
	selectors := make([]string, 0, 2)
	for _, account := range []string{first, second} {
		result, err := provider.ZCodeAccountOperation(ctx, pid, account, "context", nil)
		if err != nil {
			t.Fatal(err)
		}
		var data struct {
			AccountSelector string `json:"accountSelector"`
		}
		if err := json.Unmarshal(result.Data, &data); err != nil || data.AccountSelector == "" || data.AccountSelector == account {
			t.Fatalf("unusable selector for %s: %s, %v", account, result.Data, err)
		}
		selectors = append(selectors, data.AccountSelector)
		if account == second {
			fenced := provider.WithZCodeAccountExpectation(ctx, result.Context.Scope, result.Context.Generation)
			if _, err := provider.ZCodeAccountOperation(fenced, pid, second, "execution.draw", map[string]string{"taskId": "handoff", "model": "GLM-5.3"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.EqualFold(selectors[0], selectors[1]) {
		t.Fatalf("ambiguous selectors: %v", selectors)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+pid+`/GLM-5.3-Highspeed","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set(SessionHeader, "handoff")
	r.Header.Set(AccountHeader, url.PathEscape(selectors[1]))
	w := httptest.NewRecorder()
	New().Handler().ServeHTTP(w, r)
	calls := vendor.Calls("/api/v1/highspeed/anthropic/v1/messages")
	if w.Code != http.StatusOK || len(calls) != 1 || calls[0].Auth != "Bearer fixture-zcode-b" {
		t.Fatalf("second account handoff: HTTP %d %s calls=%+v", w.Code, w.Body.String(), calls)
	}
}

func TestZCodeContextPercentSelectorsPinOnlyTheirAdmittedAccount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondUser string
	}{
		{"unrelated literal percent", "owner%40example.test"},
		{"decoded user collision", "alice%40example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			vendor := zcodefixture.Start(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, first := zcodefixture.Install(t, ctx)
			var replaced struct {
				Account string `json:"account"`
			}
			if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": first,
				"auth": zcodefixture.AuthFor("alice@example.test", "fixture-business", "fixture-zcode")}, &replaced); err != nil || replaced.Account != first {
				t.Fatalf("first account replacement=%+v err=%v", replaced, err)
			}
			second, err := plugin.Import(ctx, "zcode", zcodefixture.AuthFor(tc.secondUser, "fixture-business-b", "fixture-zcode-b"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := plugin.Providers(ctx); err != nil {
				t.Fatal(err)
			}
			pid := provider.PluginID("zcode")
			s := New()
			for _, account := range []struct {
				key   string
				task  string
				token string
			}{
				{first, "task-alice", "fixture-zcode"},
				{second, "task-percent", "fixture-zcode-b"},
			} {
				result, err := provider.ZCodeAccountOperation(ctx, pid, account.key, "context", nil)
				if err != nil {
					t.Fatal(err)
				}
				var data struct {
					AccountSelector string `json:"accountSelector"`
				}
				if err := json.Unmarshal(result.Data, &data); err != nil || data.AccountSelector == "" {
					t.Fatalf("selector for %s: %s err=%v", account.key, result.Data, err)
				}
				fenced := provider.WithZCodeAccountExpectation(ctx, result.Context.Scope, result.Context.Generation)
				if _, err := provider.ZCodeAccountOperation(fenced, pid, account.key, "execution.draw", map[string]string{"taskId": account.task, "model": "GLM-5.3"}); err != nil {
					t.Fatalf("admission for %s: %v", data.AccountSelector, err)
				}
				vendor.SetNativeSuccessAuth(account.token)
				before := len(vendor.Calls("/api/v1/highspeed/anthropic/v1/messages"))
				r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+pid+`/GLM-5.3-Highspeed","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
				r.Header.Set("Anthropic-Version", "2023-06-01")
				r.Header.Set(SessionHeader, account.task)
				r.Header.Set(AccountHeader, data.AccountSelector)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				calls := vendor.Calls("/api/v1/highspeed/anthropic/v1/messages")
				if w.Code != http.StatusOK || len(calls) != before+1 || calls[before].Auth != "Bearer "+account.token {
					t.Fatalf("pinned %q: HTTP %d %s calls=%+v", data.AccountSelector, w.Code, w.Body.String(), calls)
				}
			}
		})
	}
}

func TestZCodeScopedGatewayRejectsReloginBeforeSpending(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	vendor := zcodefixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := zcodefixture.Install(t, ctx)
	_, secrets := newCaller(t, "Full")
	h := lanGuard(New().Handler())
	post := func(path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
		r.Header.Set("Authorization", "Bearer "+secrets[0])
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	initial := post(provider.RemoteZCodeAccountPath, map[string]any{"provider": "zcode", "account": account, "operation": "context"})
	var captured plugin.AccountResult
	if initial.Code != http.StatusOK || json.Unmarshal(initial.Body.Bytes(), &captured) != nil {
		t.Fatalf("context: %d %s", initial.Code, initial.Body.String())
	}
	var imported struct{ Account string }
	if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": account,
		"auth": zcodefixture.Auth("fixture-business-new", "fixture-zcode-new")}, &imported); err != nil || imported.Account != account {
		t.Fatalf("relogin=%+v err=%v", imported, err)
	}
	draw := map[string]any{"provider": "zcode", "account": account, "operation": "execution.draw",
		"params": map[string]string{"taskId": "handoff", "model": "GLM-5.3"}, "scope": captured.Context.Scope, "generation": captured.Context.Generation}
	reset := map[string]any{"provider": "zcode", "account": account, "window": "week", "scope": captured.Context.Scope, "generation": captured.Context.Generation}
	for _, tc := range []struct {
		path string
		body any
	}{{provider.RemoteZCodeAccountPath + "/scoped", draw}, {provider.RemoteZCodeResetPath + "/scoped", reset}} {
		w := post(tc.path, tc.body)
		if w.Code < 400 || strings.Contains(w.Body.String(), "fixture-zcode") {
			t.Fatalf("stale request accepted or leaked a credential: %d %s", w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/highspeed/draw", "/api/v1/coding-plan/reset/status", "/api/v1/coding-plan/reset/use"} {
		if calls := vendor.Calls(path); len(calls) != 0 {
			t.Fatalf("stale request contacted %s: %+v", path, calls)
		}
	}
}
