package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// The public provider and accounts commands perform the Console device
// flow, rather than saving an empty key or asking a plugin to sign in.
func TestOpenCodeOAuthCLI(t *testing.T) {
	groupsHome(t)
	s := settings.Load()
	s.SignInLinkOnly = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var issued int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/console/auth/device/code":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["client_id"] != "opencode-cli" || body["supports_org_scope"] != true {
				t.Errorf("device authorization: %v, %v", body, err)
				http.Error(w, "bad device request", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"device_code": "device", "user_code": "ABCD-EFGH", "verification_uri_complete": "/console/device?user_code=ABCD-EFGH", "expires_in": 60, "interval": 0})
		case "/console/auth/device/token":
			mu.Lock()
			issued++
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"access_token": "oauth-access", "refresh_token": "oauth-refresh", "expires_in": 3600, "org_id": "org-work"})
		case "/console/api/user":
			if r.Header.Get("Authorization") != "Bearer oauth-access" {
				t.Errorf("Console identity sent %q", r.Header.Get("Authorization"))
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "user-member", "email": "member@example.com"})
		case "/console/api/orgs":
			json.NewEncoder(w).Encode([]map[string]string{{"id": "org-work", "name": "Workspace"}})
		case "/console/api/v2/config":
			json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{}})
		default:
			if strings.HasSuffix(r.URL.Path, "/models") {
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "glm-5.3"}}})
				return
			}
			http.Error(w, "unexpected test request", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Cleanup(provider.OpenCodeOAuthForTest(srv.URL+"/console", srv.URL))

	for _, id := range []string{"opencode-go", "opencode-zen"} {
		t.Run(id, func(t *testing.T) {
			out, err := printed(t, func() error { return providerCmd([]string{"provider", "add", id}) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "ABCD-EFGH") || !strings.Contains(out, "member@example.com") || strings.Contains(out, "oauth-refresh") || strings.Contains(out, "oauth-access") {
				t.Fatalf("device sign-in output: %q", out)
			}
			p, err := provider.Find(id)
			if err != nil || p.Account == nil || p.Account.Agent != id || p.Key != "" {
				t.Fatalf("OAuth account not served after provider add: provider=%+v, error=%v", p, err)
			}
			ls := provider.Logins(id)
			if len(ls) != 1 || !ls[0].On {
				t.Fatalf("signed-in account not usable: %+v", ls)
			}
			for _, args := range [][]string{{"provider", "key", id, "wrong-key"}, {"provider", "set", id, "key=wrong-key"}} {
				if err := providerCmd(args); err == nil || !strings.Contains(err.Error(), "OAuth") {
					t.Fatalf("key mutation on an OAuth account: %v", err)
				}
			}
			user := ls[0].User
			if _, err := printed(t, func() error { return accountsCmd([]string{"accounts", "add", id}) }); err != nil {
				t.Fatal(err)
			}
			if ls = provider.Logins(id); len(ls) != 1 || ls[0].User != user {
				t.Fatalf("signing in again duplicated the account: %+v", ls)
			}
			if _, err := printed(t, func() error { return accountsCmd([]string{"accounts", "switch", id, user}) }); err != nil {
				t.Fatal(err)
			}
			if _, err := printed(t, func() error { return accountsCmd([]string{"accounts", "forget", id, user}) }); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Find(id); err == nil {
				t.Fatal("forgotten OAuth account remains served")
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if issued != 4 {
		t.Fatalf("issued %d tokens for four completed device sign-ins", issued)
	}
}

func TestOpenCodeOAuthRejectsKeyArguments(t *testing.T) {
	groupsHome(t)
	for _, id := range []string{"opencode-go", "opencode-zen"} {
		p, err := provider.FromPreset(id)
		if err != nil {
			t.Fatal(err)
		}
		p.Key = "existing-key"
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(provider.Path())
		if err != nil {
			t.Fatal(err)
		}
		for _, arg := range []string{"replacement-key", "key=replacement-key", "models=glm-5.3"} {
			err := providerCmd([]string{"provider", "add", id, arg})
			if err == nil || !strings.Contains(err.Error(), "OAuth") {
				t.Fatalf("add %s %s: %v, want OAuth instruction", id, arg, err)
			}
			after, err := os.ReadFile(provider.Path())
			if err != nil || string(after) != string(before) {
				t.Fatalf("rejected OAuth add changed existing keys/settings: %v", err)
			}
		}
	}
}
