package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

const destinationPlugin = "fetch-destination"
const destinationChat = `{"model":"chat","messages":[{"role":"user","content":"fixture"}]}`

func TestPluginFetchUsesOneLoaderForAddressAndKey(t *testing.T) {
	for _, fromModels := range []bool{false, true} {
		name := "loader reads credentials"
		if fromModels {
			name = "loader reads account model URL"
		}
		t.Run(name, func(t *testing.T) {
			testPluginFetchAfterPeerSignIn(t, fromModels)
		})
	}
}

func testPluginFetchAfterPeerSignIn(t *testing.T, fromModels bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var oldRequests, newRequests atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			oldRequests.Add(1)
		}
		w.WriteHeader(http.StatusMisdirectedRequest)
	}))
	t.Cleanup(old.Close)
	current := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newRequests.Add(1)
		if r.URL.RequestURI() != "/tenant-b/chat/completions?probe=yes" || r.Header.Get("Authorization") != "Bearer fixture-key-b" {
			t.Errorf("replacement destination received %s with %q", r.URL.RequestURI(), r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	signIn := destinationSignIn("fixture-key-a", old.URL+"/tenant-a")
	metadata := signIn["metadata"].(map[string]string)
	if fromModels {
		metadata["useModelsBase"] = "true"
	}
	p := destinationProvider(t, ctx, old.URL, signIn)
	if origin := p.Origin(ctx); origin != old.URL {
		t.Fatalf("initial Origin = %q, want %q", origin, old.URL)
	}
	signIn["key"] = "fixture-key-b"
	metadata["baseURL"] = current.URL + "/tenant-b"
	replaceDestinationSignIn(t, signIn)

	result, err := doDestinationRequest(ctx, p, pluginBase+destinationPlugin+"/v1/chat/completions?probe=yes", "", destinationChat)
	if err != nil || result != http.StatusNoContent {
		t.Errorf("Provider.Do after peer replaces the sign-in = %d, %v, want 204", result, err)
	}
	if oldRequests.Load() != 0 || newRequests.Load() != 1 {
		t.Errorf("destination call counts = old %d, new %d, want 0 and 1", oldRequests.Load(), newRequests.Load())
	}
	if origin := p.Origin(ctx); origin != current.URL {
		t.Errorf("Origin after peer sign-in = %q, want %q", origin, current.URL)
	}
}

func TestPluginFetchStopsWhenSignInChangesDuringHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var vendorRequests atomic.Int32
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	hook, entered, release := holdDestinationSetup(t, ctx)
	t.Setenv("DESTINATION_HEADERS", hook)
	base := vendor.URL + "/v1"
	p := destinationProvider(t, ctx, base, destinationSignIn("fixture-key", base))
	type result struct {
		status int
		err    error
	}
	answers := make(chan result, 1)
	go func() {
		status, err := doDestinationRequest(ctx, p, pluginBase+destinationPlugin+"/v1/chat/completions", "held-headers", destinationChat)
		answers <- result{status, err}
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-answers
		}
	}()
	select {
	case <-entered:
	case got := <-answers:
		joined = true
		t.Fatalf("request ended before the header hook: %+v", got)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := plugin.Restore(ctx, destinationPlugin, destinationPlugin, destinationSignIn("fixture-key", base)); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case got := <-answers:
		joined = true
		if got.err == nil || !strings.Contains(got.err.Error(), "sign-in changed") {
			t.Errorf("stale request = %+v, want a sign-in change before send", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if vendorRequests.Load() != 0 {
		t.Errorf("request reached the vendor after its sign-in was replaced %d times", vendorRequests.Load())
	}
	status, err := doDestinationRequest(ctx, p, pluginBase+destinationPlugin+"/v1/chat/completions", "", destinationChat)
	if err != nil || status != http.StatusNoContent || vendorRequests.Load() != 1 {
		t.Errorf("fresh request = %d, %v, calls %d, want 204 and one send", status, err, vendorRequests.Load())
	}
}

func TestPluginFetchOnlyUsesTheSelectedOriginForAbsoluteURLs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var calls, otherCalls atomic.Int32
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RequestURI() != "/outside?kind=search" {
			t.Errorf("absolute path = %q", r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			otherCalls.Add(1)
		}
		w.WriteHeader(http.StatusMisdirectedRequest)
	}))
	t.Cleanup(other.Close)
	p := destinationProvider(t, ctx, vendor.URL+"/fallback", destinationSignIn("fixture-key", vendor.URL+"/v1"))
	status, err := doDestinationRequest(ctx, p, vendor.URL+"/outside?kind=search", "", destinationChat)
	if err != nil || status != http.StatusNoContent || calls.Load() != 1 {
		t.Errorf("same-origin absolute request = %d, %v, calls = %d", status, err, calls.Load())
	}
	_, err = doDestinationRequest(ctx, p, other.URL+"/outside", "", destinationChat)
	if err == nil || !strings.Contains(err.Error(), "plugin sends to") || otherCalls.Load() != 0 {
		t.Errorf("foreign-origin absolute request = %v, calls = %d", err, otherCalls.Load())
	}
}

func TestPluginFetchKeepsGeminiPathAndUnwrapsCodeAssist(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var calls atomic.Int32
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}` {
			t.Errorf("Gemini body = %s, %v", body, err)
		}
		if r.URL.RequestURI() != "/v1beta/models/gem:streamGenerateContent?alt=sse" || r.Header.Get("X-Goog-Api-Key") != "fixture-key" {
			t.Errorf("Gemini path or key = %s, %q", r.URL.RequestURI(), r.Header.Get("X-Goog-Api-Key"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	p := destinationProvider(t, ctx, vendor.URL+"/fallback", destinationSignIn("fixture-key", ""))
	body := `{"model":"gem","request":{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}}`
	status, err := doDestinationRequest(ctx, p, pluginBase+destinationPlugin+"/v1internal:streamGenerateContent?alt=sse", "", body)
	if err != nil || status != http.StatusNoContent || calls.Load() != 1 {
		t.Errorf("Gemini request = %d, %v, calls = %d", status, err, calls.Load())
	}
}

func TestPublicPluginFetchUsesItsFallbackWithDiscoveryOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var calls atomic.Int32
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/fallback/chat/completions" || r.Header.Get("Authorization") != "Bearer public" {
			t.Errorf("public request = %s, %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if route := r.Header.Get("X-Header-Route"); route != "new" {
			t.Errorf("public request used option %q, want the saved route", route)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Setenv("DESTINATION_PUBLIC", "yes")
	destinationProvider(t, ctx, vendor.URL+"/fallback", destinationSignIn("public", ""))
	saved := settings.Load()
	saved.NoAccountDiscovery = true
	saved.PluginOptions = map[string]map[string]bool{destinationPlugin: {"new-route": true}}
	if err := settings.Save(saved); err != nil {
		t.Fatal(err)
	}
	p, err := Find(PluginID(destinationPlugin))
	if err != nil {
		t.Fatal(err)
	}
	status, err := doDestinationRequest(ctx, *p, pluginBase+destinationPlugin+"/v1/chat/completions", "", destinationChat)
	if err != nil || status != http.StatusNoContent || calls.Load() != 1 {
		t.Errorf("public request = %d, %v, calls = %d", status, err, calls.Load())
	}
}

func destinationSignIn(key, base string) map[string]any {
	return map[string]any{"type": "api", "key": key, "metadata": map[string]string{"baseURL": base}}
}

func destinationProvider(t *testing.T, ctx context.Context, fallback string, signIn map[string]any) Provider {
	t.Helper()
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("DESTINATION_FALLBACK", fallback)
	t.Cleanup(plugin.Settle)
	entry, err := filepath.Abs("../plugin/testdata/fetch-destination/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Restore(ctx, destinationPlugin, destinationPlugin, signIn); err != nil {
		t.Fatal(err)
	}
	saveLogins(t, savedLogin{Agent: "plugin:" + destinationPlugin, User: "Fixture", Home: destinationPlugin, On: true})
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := Find(PluginID(destinationPlugin))
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsPlugin() || p.PluginAccountKey() != destinationPlugin {
		t.Fatalf("expected the fixture's plugin account, got %s (%s)", p.ID, p.PluginAccountKey())
	}
	return *p
}

func replaceDestinationSignIn(t *testing.T, signIn map[string]any) {
	t.Helper()
	signIn["_magpieAuthEpoch"] = "fixture-peer-published"
	data, err := json.Marshal(map[string]any{destinationPlugin: signIn})
	if err != nil {
		t.Fatal(err)
	}
	path := plugin.AuthPath()
	temp := path + ".peer"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}

func doDestinationRequest(ctx context.Context, p Provider, url, session, payload string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set(ConversationHeader, session)
	res, err := p.Do(http.DefaultClient, req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, res.Body)
	return res.StatusCode, err
}
