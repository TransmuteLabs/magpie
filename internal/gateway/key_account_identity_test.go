package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

func TestRestrictedAccountSelectionRequiresCurrentIdentity(t *testing.T) {
	fresh(t)
	path := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	healthy := []byte(`[{"agent":"codex","user":"reader@example.test","id":"reader-id"}]`)
	write := func(contents []byte) {
		t.Helper()
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	write(healthy)
	p := provider.Provider{ID: "codex", Account: &provider.Account{Agent: "codex", User: "reader@example.test"}}
	selected := candidate{p: p, model: "sample"}
	who := access.Identity{Accounts: []string{"codex/reader-id"}}
	group := provider.Group{Name: "Sample", Accounts: who.Accounts}
	member := provider.Member{Account: who.Accounts[0]}
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses/compact", nil)
	req = req.WithContext(access.WithIdentity(req.Context(), who))

	if got := p.AccountID(); got != "reader-id" {
		t.Fatalf("display account ID = %q", got)
	}
	release := provider.Hold()
	defer release()

	check := func(phase string, allowed bool) {
		t.Helper()
		current := provider.CurrentAccountIDs(t.Context())
		plan := planned{
			held:      []candidate{selected},
			capped:    []candidate{selected},
			unlisted:  []candidate{selected},
			unlistedW: []Weighed{{}},
		}
		retained, filtered, _ := allowedCandidates(t.Context(), who, []candidate{selected}, plan, map[string]bool{"codex/sample": true})
		_, compactHeld := compactSigninHeld(req, "sample", &p)
		for path, got := range map[string]bool{
			"candidate": accountAllowed(who, selected.p, current),
			"plan":      len(retained) == 1,
			"held":      len(filtered.held) == 1,
			"capped":    len(filtered.capped) == 1,
			"unlisted":  len(filtered.unlisted) == 1,
			"group":     groupHeld(selected, []provider.Group{group}, current) == "",
			"member":    memberHeld(selected, member, group, current) == "",
			"compact":   !compactHeld,
		} {
			if got != allowed {
				t.Errorf("%s: %s allowed = %v, want %v", phase, path, got, allowed)
			}
		}
	}
	check("current ID", true)
	write([]byte(`[`))
	check("unavailable current ID", false)
	withoutAccount := access.Identity{Accounts: []string{"codex/!reader-id"}}
	if accountAllowed(withoutAccount, p, provider.CurrentAccountIDs(t.Context())) {
		t.Error("an unknown identity passed an account exclusion")
	}
	unrelated := access.Identity{Accounts: []string{"custom/another-id"}}
	if !accountAllowed(unrelated, p, func(provider.Provider) (string, bool) {
		t.Error("resolved an account with no restrictions on its provider")
		return "", false
	}) {
		t.Error("an unrelated account restriction refused the provider")
	}
	write(healthy)
	check("readable again", true)
}

func TestRestrictedGatewayUsesCurrentAccountIdentity(t *testing.T) {
	heads := twoAccounts(t)
	server := New().Handler()
	keys, secrets := newCaller(t, "Held")
	if _, err := access.Update("accounts-key", access.Change{
		Key: keys[0].ID, Accounts: []string{"codex/spare@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"codex/gpt-5.5","stream":true,"input":"ping"}`))
		req.Header.Set("Authorization", "Bearer "+secrets[0])
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		return res
	}
	expectSpare := func(phase string) {
		t.Helper()
		*heads = nil
		res := call()
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "from acct-2") ||
			len(*heads) != 1 || (*heads)[0].Get("Chatgpt-Account-Id") != "acct-2" {
			t.Fatalf("%s: status %d, upstream calls %d, body %s", phase, res.Code, len(*heads), res.Body.String())
		}
	}
	expectSpare("valid current records")
	path := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	loginFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	release := provider.Hold()
	defer release()
	if err := os.WriteFile(path, []byte(`[`), 0o600); err != nil {
		t.Fatal(err)
	}
	*heads = nil
	res := call()
	if res.Code != http.StatusForbidden || len(*heads) != 0 {
		t.Errorf("damaged current records: status %d, upstream calls %d, body %s", res.Code, len(*heads), res.Body.String())
	}
	if err := os.WriteFile(path, loginFile, 0o600); err != nil {
		t.Fatal(err)
	}
	expectSpare("current records repaired")
}
