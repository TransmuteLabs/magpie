package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/plugin"
	fixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestZCodeAccountTaskHeadersAreValidatedBeforeRemoteDispatch(t *testing.T) {
	sandboxHome(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != provider.RemoteZCodeAccountPath+"/scoped" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"context":{"scope":"scope","generation":"epoch"},"data":{}}`))
	}))
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "fixture", Chat: server.URL}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	usageRoutes(mux, folderOnly{})
	cases := []struct {
		id string
		ok bool
	}{
		{"", false}, {" leading", false}, {"trailing ", false}, {"a\nb", false}, {"a\x7fb", false},
		{strings.Repeat("a", 128), true}, {strings.Repeat("a", 129), false},
		{strings.Repeat("é", 64), true}, {strings.Repeat("é", 65), false},
	}
	for _, operation := range []string{"execution.take", "execution.draw", "execution.continue"} {
		for _, tc := range cases {
			body, err := json.Marshal(map[string]any{"provider": "office/zcode", "account": "alice", "operation": operation,
				"scope": "scope", "generation": "epoch", "params": map[string]any{"taskId": tc.id, "model": "GLM-5.3"}})
			if err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/usage/zcode-account", bytes.NewReader(body)))
			if tc.ok {
				if w.Code != http.StatusOK || calls.Load() != before+1 {
					t.Errorf("%s task %q: status=%d calls=%d body=%s", operation, tc.id, w.Code, calls.Load()-before, w.Body.String())
				}
			} else if w.Code < 400 || calls.Load() != before {
				t.Errorf("%s invalid task %q dispatched: status=%d calls=%d", operation, tc.id, w.Code, calls.Load()-before)
			}
		}
	}
}

func TestZCodeAccountScopedRemoteNeverRetriesDownlevel(t *testing.T) {
	sandboxHome(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != provider.RemoteZCodeAccountPath+"/scoped" {
			t.Errorf("unfenced request: %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "fixture", Chat: server.URL}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	usageRoutes(mux, folderOnly{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/usage/zcode-account", strings.NewReader(`{"provider":"office/zcode","account":"alice","operation":"execution.draw","params":{"taskId":"chat","model":"GLM-5.3"},"scope":"scope","generation":"epoch"}`)))
	if w.Code < 400 || calls.Load() != 1 {
		t.Fatalf("downlevel response=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
}

func TestZCodeResetScopedRemoteNeverRetriesDownlevel(t *testing.T) {
	sandboxHome(t)
	var accountCalls, resetCalls, oldCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case provider.RemoteZCodeAccountPath + "/scoped":
			accountCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"context":{"scope":"scope","generation":"epoch"},"data":{}}`))
		case provider.RemoteZCodeResetPath + "/scoped":
			resetCalls.Add(1)
			http.NotFound(w, r)
		default:
			oldCalls.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "fixture", Chat: server.URL}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	usageRoutes(mux, folderOnly{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/usage/zcode-account", strings.NewReader(`{"provider":"office/zcode","account":"alice","operation":"reset.use","params":{"window":"week"},"scope":"scope","generation":"epoch"}`)))
	if w.Code < 400 || accountCalls.Load() != 1 || resetCalls.Load() != 1 || oldCalls.Load() != 0 {
		t.Fatalf("downlevel reset response=%d context=%d strict=%d old=%d body=%s", w.Code, accountCalls.Load(), resetCalls.Load(), oldCalls.Load(), w.Body.String())
	}
}

func TestZCodeAccountLocalAndStrictRemotePendingAndScopeChange(t *testing.T) {
	vendor := fixture.Start(t)
	sandboxHome(t)
	t.Setenv("MAGPIE_ADDR", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := fixture.Install(t, ctx)
	s := settings.Load()
	s.LAN = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	secret, err := access.Update("add-key", access.Change{Name: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(gateway.New().Handler())
	t.Cleanup(remote.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: secret, Chat: remote.URL}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	zcodeAccountRoutes(mux)
	post := func(id, operation string, params any, scope, generation string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{"provider": id, "account": account, "operation": operation, "params": params, "scope": scope, "generation": generation})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/usage/zcode-account", bytes.NewReader(body)))
		return w
	}
	local := provider.PluginID("zcode")
	first := post(local, "context", nil, "", "")
	var captured plugin.AccountResult
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &captured) != nil || captured.Context.Scope == "" || captured.Context.Generation == "" {
		t.Fatalf("context: %d %s", first.Code, first.Body.String())
	}
	scope, generation := captured.Context.Scope, captured.Context.Generation
	vendor.LoseResetReply(true)
	lost := post(local, "reset.use", map[string]string{"window": "five-hour"}, scope, generation)
	var failure struct {
		State provider.ZCodeResetState `json:"state"`
	}
	if lost.Code < 400 || json.Unmarshal(lost.Body.Bytes(), &failure) != nil || failure.State.Pending != "unconfirmed" {
		t.Fatalf("lost response: %d %s", lost.Code, lost.Body.String())
	}
	vendor.ResetStatusUnavailable(true)
	for _, id := range []string{local, "office/" + local} {
		w := post(id, "reset.status", nil, scope, generation)
		var result struct {
			Data provider.ZCodeResetState `json:"data"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Available || result.Data.Pending != "unconfirmed" {
			t.Fatalf("%s lost pending state: %d %s", id, w.Code, w.Body.String())
		}
	}
	vendor.ResetStatusUnavailable(false)
	for _, id := range []string{local, "office/" + local} {
		w := post(id, "reset.status", nil, scope, generation)
		var result struct {
			Data provider.ZCodeResetState `json:"data"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Pending != "observed-completion" {
			t.Fatalf("%s lost observed uncertainty: %d %s", id, w.Code, w.Body.String())
		}
	}
	var imported struct {
		Account string `json:"account"`
	}
	if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": account, "auth": fixture.Auth("replacement-business", "replacement-jwt")}, &imported); err != nil || imported.Account != account {
		t.Fatalf("relogin: %+v %v", imported, err)
	}
	before := len(vendor.Calls("/api/v1/coding-plan/reset/use"))
	for _, id := range []string{local, "office/" + local} {
		w := post(id, "reset.use", map[string]string{"window": "week"}, scope, generation)
		if w.Code < 400 {
			t.Fatalf("%s accepted stale confirmation: %d %s", id, w.Code, w.Body.String())
		}
	}
	if after := len(vendor.Calls("/api/v1/coding-plan/reset/use")); before != 1 || after != before {
		t.Fatalf("stale or uncertain debit retried: before=%d after=%d", before, after)
	}
}
