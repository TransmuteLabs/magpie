package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

// openCounter is a desktop that counts the pages it was asked to open.
type openCounter struct {
	webHost
	mu     sync.Mutex
	opened []string
}

func (w *openCounter) OpenURL(u string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opened = append(w.opened, u)
	return nil
}

// A plugin's sign-in page opens once. Grok Build's `grok login` opens its
// page in the browser itself on macOS and Windows, and magpie opened it
// again: two pages every time (andyfmyth on X). A plugin that says the page
// is open already gets none opened by magpie; one that doesn't gets one.
func TestPluginSignInPageOpensOnce(t *testing.T) {
	bun := testenv.Bun(t)
	dir := t.TempDir()
	testenv.SetHome(t, dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Cleanup(plugin.Settle)
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(t.Context(), abs); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(t.Context()); err != nil {
		t.Fatal(err)
	}
	start := func(team string) (int, provider.SignInState) {
		t.Helper()
		w := &openCounter{}
		body, _ := json.Marshal(map[string]any{"provider": "fakeco", "method": 1, "inputs": map[string]string{"where": "work", "team": team}})
		rec := httptest.NewRecorder()
		Handler(w, nil).ServeHTTP(rec, httptest.NewRequest("POST", "/api/plugin-signin", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("sign-in: %d %s", rec.Code, rec.Body)
		}
		var st provider.SignInState
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { provider.CancelSignIn(st.ID) })
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.opened), st
	}
	if n, st := start("cli-opened"); n != 0 || !st.Opened || st.URL == "" {
		t.Fatalf("a page the plugin's CLI opened was opened %d more times: %+v", n, st)
	}
	if n, st := start("blue"); n != 1 || st.Opened {
		t.Fatalf("a page nothing opened was opened %d times: %+v", n, st)
	}
}

func TestPluginBrowserSignInSurvivesRequest(t *testing.T) {
	bun := testenv.Bun(t)
	home := sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Cleanup(plugin.Settle)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	source, err := filepath.Abs("../plugin/testdata/signinend/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	requestCtx, endRequest := context.WithCancel(ctx)
	defer endRequest()
	reply := do(Handler(nil, nil), httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/api/plugin-signin",
		bytes.NewBufferString(`{"provider":"signinend","method":1}`)))
	endRequest()
	var started provider.SignInState
	if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &started) != nil || started.ID == "" || started.State != "waiting" {
		t.Fatalf("browser setup: %d %s", reply.Code, reply.Body)
	}
	t.Cleanup(func() { provider.CancelSignIn(started.ID) })
	if err := os.WriteFile(filepath.Join(home, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := provider.WaitSignIn(ctx, started.ID)
	if err != nil || result.State != "done" || !plugin.SignedIn("signinend") {
		t.Fatalf("browser did not finish after its start request ended: %+v (%v)", result, err)
	}
}
