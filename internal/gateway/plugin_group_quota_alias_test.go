package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

func TestPluginGroupCapUsesProviderModelIDs(t *testing.T) {
	bun := testenv.Bun(t)
	fresh(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("FAKE_GLM", "1")
	t.Setenv("FAKE_SENSENOVA_QUOTA", "1")
	windowsPath := filepath.Join(t.TempDir(), "windows.json")
	t.Setenv("FAKE_WINDOWS", windowsPath)
	t.Cleanup(plugin.Settle)
	old := allowances
	allowances = provider.Allowances
	t.Cleanup(func() { allowances = old })

	var mu sync.Mutex
	var tried []string
	testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		tried = append(tried, req.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chatSSEReply("from "+req.Model))
	}))

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	path, err := filepath.Abs("../plugin/testdata/fake/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, path); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	signIn, err := provider.StartPluginSignIn("fakeco", 1, map[string]string{"where": "work", "team": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SubmitSignInCallback(signIn.ID, "good"); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	for native, alias := range map[string]string{"glm-5.3": "paid-alias", "fake-1": "free-alias"} {
		if _, changed, err := provider.SetModelID("fakeco/"+native, alias); err != nil || !changed {
			t.Fatalf("set model ID %s: changed %v, error %v", alias, changed, err)
		}
	}
	if err := provider.SetAccountCap("fakeco", "a@fake", 90); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, name string, members []string, windows []map[string]any, want string) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"a@fake": windows})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(windowsPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if !provider.ReadUsageNow(ctx, "plugin:fakeco", "a@fake") {
			t.Fatal("could not refresh the account's allowance")
		}
		al := provider.Allowances("plugin:fakeco")["a@fake"]
		if len(al) != len(windows) || al[0].Name != windows[0]["name"] || al[0].Used != 100 {
			t.Fatalf("wrong allowance: %+v", al)
		}
		if err := provider.SaveGroup(provider.Group{Name: name, Routing: provider.Ordered, Members: members}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		tried = nil
		mu.Unlock()
		code, body := postTo(t, New(), "/v1/chat/completions", `{"model":"group/`+name+`","messages":[{"role":"user","content":"hi"}],"stream":true}`)
		mu.Lock()
		seen := slices.Clone(tried)
		mu.Unlock()
		if want != "" {
			if code != 200 || !slices.Equal(seen, []string{want}) || !strings.Contains(body, "from "+want) {
				t.Errorf("want only %s to answer: status %d, upstream %v, body %s", want, code, seen, body)
			}
		} else if code != 429 || len(seen) != 0 || !strings.Contains(body, "usage cap reached") {
			t.Errorf("want both models held: status %d, upstream %v, body %s", code, seen, body)
		}
	}

	for _, test := range []struct {
		name     string
		field    string
		model    string
		freeFull bool
		wantFree bool
	}{
		{name: "excluded", field: "notModels", model: "fake-1", wantFree: true},
		{name: "included", field: "models", model: "glm-5.3", wantFree: true},
		{name: "own-quota-full", field: "notModels", model: "fake-1", freeFull: true},
		{name: "shared-quota-full"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			paid := map[string]any{"name": test.name, "used": 100, "resetsAt": reset, "span": 18000}
			if test.field != "" {
				paid[test.field] = []string{test.model}
			}
			windows := []map[string]any{paid}
			if test.freeFull {
				windows = append(windows, map[string]any{"name": "free quota", "used": 100, "resetsAt": reset, "span": 18000, "models": []string{"fake-1"}})
			}
			var want string
			if test.wantFree {
				want = "fake-1"
			}
			run(t, test.name, []string{"fakeco/paid-alias", "fakeco/free-alias"}, windows, want)
		})
	}

	const flash = "sensenova-6.8-flash-lite"
	raw, err := os.ReadFile("testdata/sensenova-pool-windows.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		second        string
		dedicatedFull bool
		want          string
	}{
		{name: "sensenova-dedicated-open", second: flash, want: flash},
		{name: "sensenova-dedicated-full", second: flash, dedicatedFull: true},
		{name: "sensenova-outside-general", second: "free-alias", dedicatedFull: true, want: "fake-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var windows []map[string]any
			if err := json.Unmarshal(raw, &windows); err != nil {
				t.Fatal(err)
			}
			reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			for _, window := range windows {
				window["resetsAt"] = reset
				if test.dedicatedFull && window["name"] == "Flash-Lite积分池 · 5 hours" {
					window["used"], window["amount"], window["display"] = 100, 120000, "120000 / 120000"
				}
			}
			run(t, test.name, []string{"fakeco/glm-5.2", "fakeco/" + test.second}, windows, test.want)
		})
	}
}
