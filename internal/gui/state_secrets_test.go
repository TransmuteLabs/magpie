package gui

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// Neither the live poll nor the startup copy carries proxy credentials,
// the old LAN key, the Bark key or the GitHub token.
func TestStateSendsNoCredentials(t *testing.T) {
	sandboxHome(t)
	const pass, lan, bark, gh = "hunter2-proxy-pass", "sk-magpie-statelanstatelanstatelan0123", "bark-state-key-0123", "ghp_statetokenstatetoken0123"
	h := Handler(nil, nil)
	call := func(method, path, body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("%s %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	call("POST", "/api/proxies/save", `{"name":"Office","url":"http://10.0.0.1:8080","user":"me","pass":"`+pass+`"}`)
	s := settings.Load()
	if len(s.Proxies) != 1 || s.Proxies[0].Pass != pass {
		t.Fatalf("the proxy wasn't saved: %+v", s.Proxies)
	}
	s.LANKey, s.BarkKey, s.GitHubToken = lan, bark, gh
	s.Proxy = "http://inline-primary:fixture-primary@127.0.0.1:18801"
	s.PluginProxy = "direct"
	s.PluginProxyCustom = "socks5://inline-plugin:fixture-plugin@127.0.0.1:18802"
	s.UpdateProxy = "https://inline-update:fixture-update@127.0.0.1:18803"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	saved := settings.Load()
	for _, path := range []string{"/api/state", "/api/state?last=1"} {
		body := call("GET", path, "")
		if path == "/api/state?last=1" && body == "null\n" {
			t.Fatal("no state was kept for the next start")
		}
		for _, secret := range []string{pass, lan, bark, gh, "fixture-primary", "fixture-plugin", "fixture-update"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s carries %q", path, secret)
			}
		}
	}
	after := settings.Load()
	if len(after.Proxies) != 1 || after.Proxies[0].Pass != pass || after.LANKey != lan || after.BarkKey != bark || after.GitHubToken != gh {
		t.Fatal("the credentials on disk were changed")
	}
	if after.Proxy != saved.Proxy || after.ProxyCustom != saved.ProxyCustom ||
		after.PluginProxy != saved.PluginProxy || after.PluginProxyCustom != saved.PluginProxyCustom ||
		after.UpdateProxy != saved.UpdateProxy || after.UpdateProxyCustom != saved.UpdateProxyCustom {
		t.Fatal("the inline proxies on disk were changed")
	}
}
