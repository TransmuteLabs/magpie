package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

func TestZCodeResetNeedsAFullKey(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	keys, secrets := newCaller(t, "Owner", "Viewer", "Limited")
	if _, err := access.Update("access-key", access.Change{Key: keys[1].ID, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("limit-key", access.Change{Key: keys[2].ID, Limit: &access.Limit{Period: "day", Tokens: 1000}}); err != nil {
		t.Fatal(err)
	}
	h := lanGuard(New().Handler())
	do := func(path, origin, secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
		if origin != "" {
			r.Header.Set("Origin", origin)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "cors")
		}
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	path := provider.RemoteZCodeResetPath + "?user=person@example.com&window=fiveHour&context=" + strings.Repeat("a", 64)
	for _, origin := range []string{"", "null", "tauri://localhost", "https://evil.example"} {
		w := do(path, origin, "")
		if w.Code < 400 || w.Code == http.StatusNotFound {
			t.Fatalf("unkeyed %q reached the financial endpoint: %d %s", origin, w.Code, w.Body.String())
		}
	}
	if w := do(path, "https://evil.example", secrets[0]); w.Code != http.StatusForbidden {
		t.Fatalf("foreign page with full key: %d %s", w.Code, w.Body.String())
	}
	if w := do(path, "", secrets[1]); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), ReadOnlyCode) {
		t.Fatalf("read-only: %d %s", w.Code, w.Body.String())
	}
	if w := do(path, "", secrets[2]); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), provider.KeyHeldCode) {
		t.Fatalf("limited: %d %s", w.Code, w.Body.String())
	}
	// The full key reaches the named-account validation; no vendor or real
	// credentials are needed to establish this boundary.
	if w := do(provider.RemoteZCodeResetPath, "", secrets[0]); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "name the ZCode account") {
		t.Fatalf("owner validation: %d %s", w.Code, w.Body.String())
	}
}
