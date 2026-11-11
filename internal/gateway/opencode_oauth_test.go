package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// The real gateway consumes the same account that the device flow publishes,
// including its workspace and request settings; the fake is the only vendor.
func TestOpenCodeOAuthGatewayDeviceToChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var path, auth, org, fixed, body string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/console/auth/device/code":
			io.WriteString(w, `{"device_code":"device","user_code":"CODE","verification_uri_complete":"/console/verify","expires_in":60,"interval":0}`)
		case "/console/auth/device/token":
			io.WriteString(w, `{"access_token":"gateway-access","refresh_token":"gateway-refresh","expires_in":3600,"org_id":"org-1"}`)
		case "/console/api/user":
			io.WriteString(w, `{"id":"user-1","email":"person@example.com"}`)
		case "/console/api/orgs":
			io.WriteString(w, `[{"id":"org-1","name":"Workspace"}]`)
		case "/zen/go/v1/usage":
			io.WriteString(w, `{"usage":{"rolling":{"percent":0}}}`)
		default:
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				http.Error(w, "unexpected path", http.StatusNotFound)
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			path, auth, org, fixed, body = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Opencode-Org-Id"), r.Header.Get("X-Fixed"), string(b)
			mu.Unlock()
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"from oauth"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}
	}))
	t.Cleanup(vendor.Close)
	t.Cleanup(provider.OpenCodeOAuthForTest(vendor.URL+"/console", vendor.URL))
	st, err := provider.StartSignIn("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err = provider.WaitSignIn(ctx, st.ID)
	if err != nil || st.State != "done" {
		t.Fatalf("sign-in %+v: %v", st, err)
	}
	p, err := provider.Find("opencode-go")
	if err != nil || p.Account == nil {
		t.Fatalf("provider %+v: %v", p, err)
	}
	p.Models = []string{"test-model"}
	p.Headers = map[string]string{"X-Fixed": "from-picks", "X-Org-Id": "not-the-workspace"}
	p.ExtraBody = provider.ExtraBody{"temperature": json.RawMessage(`0.4`)}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	s := New()
	list := httptest.NewRecorder()
	s.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"opencode-go/test-model"`) {
		t.Fatalf("model list %d: %s", list.Code, list.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"opencode-go/test-model","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from oauth") {
		t.Fatalf("gateway %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if path != "/inference/go/openai/v1/chat/completions" || auth != "Bearer gateway-access" || org != "org-1" || fixed != "from-picks" || !strings.Contains(body, `"temperature":0.4`) {
		t.Fatalf("upstream path=%q auth=%q org=%q fixed=%q body=%s", path, auth, org, fixed, body)
	}
}
