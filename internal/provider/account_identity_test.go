package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCurrentAccountIdentityUsesCurrentRecords(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	path := loginsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(p Provider, id string, known bool) {
		t.Helper()
		if got, ok := CurrentAccountIDs(t.Context())(p); got != id || ok != known {
			t.Errorf("%s: ID %q, known %t; want %q, %t", p.ID, got, ok, id, known)
		}
	}
	native := Provider{ID: "codex", Account: &Account{Agent: "codex", User: "person@example.test"}}
	fallbackID := loginID(savedLogin{Agent: "codex", User: native.Account.User})
	check(native, fallbackID, true)
	write(`[{"agent":"codex","user":"person@example.test"}]`)
	check(native, fallbackID, true)
	write(`[{"agent":"codex","user":"person@example.test","id":"stable-id"}]`)
	check(native, "stable-id", true)
	spare := Provider{ID: ChatGPTAPIID, Account: &Account{Agent: ChatGPTAPIID, User: "saved@example.test"}}
	check(spare, "", false)

	community := PluginForTest("demo", "demo")
	community.Account.User, community.Account.pluginKey = "demo-account", "demo#synthetic"
	community.Account.codeAssist = "https://plugin.example.test"
	write(`[{"agent":"plugin:demo","home":"demo#synthetic","user":"demo-account"}]`)
	check(community, loginID(savedLogin{Agent: "demo", User: "demo-account"}), true)
	const legacy = `[{"agent":"demo","user":"demo-account","id":"legacy-id"},{"agent":"plugin:demo","home":"demo#synthetic","user":"demo-account"}]`
	write(legacy)
	check(community, "legacy-id", true)
	write(`[{"agent":"demo","user":"demo-account","id":"legacy-id"},{"agent":"plugin:demo","home":"demo#different","user":"demo-account"}]`)
	check(community, "", false)
	write(`[`)
	check(community, "", false)
	check(native, "", false)
	check(Provider{Key: "synthetic"}, KeyID("synthetic"), true)
	check(Provider{}, KeyID(""), true)

	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	community.Account.plugin.Methods = []plugin.Method{{Type: "api", Keyless: true}}
	check(community, loginID(savedLogin{Agent: "demo", User: "demo-account"}), true)
}

func TestCurrentAccountIdentityUsesPrivateSDKSelection(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	const publicID = "transferred-subscription"
	usageTestSubscriptionOwner(t, host, publicID)
	manifest := host.manifest
	scope := sdk.Scope{Plugin: manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	owners, err := host.Store().SubscriptionOwners(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identity := (extensionSubscriptionOwners{rows: owners}).identity(scope)
	p := extensionProvider(&extensionAccount{
		host: host, manifest: manifest, descriptor: manifest.Providers[0], scope: scope, public: identity,
	})
	if id, known := CurrentAccountIDs(t.Context())(p); id != "login-a" || !known {
		t.Fatalf("transferred account: ID %q, known %t", id, known)
	}
	alteredScope := scope
	alteredScope.Account = "ALICE"
	missing := extensionProvider(&extensionAccount{
		host: host, manifest: manifest, descriptor: manifest.Providers[0], scope: alteredScope, public: identity,
	})
	if id, known := CurrentAccountIDs(t.Context())(missing); id != "" || known {
		t.Errorf("unknown transferred account: ID %q, known %t", id, known)
	}
	addedScope := scope
	addedScope.Account = "bob"
	added := extensionProvider(&extensionAccount{
		host: host, manifest: manifest, descriptor: manifest.Providers[0], scope: addedScope,
		public: (extensionSubscriptionOwners{rows: owners}).identity(addedScope),
	})
	if id, known := CurrentAccountIDs(t.Context())(added); id != loginID(savedLogin{Agent: publicID, User: "bob"}) || !known {
		t.Errorf("new account of transferred provider: ID %q, known %t", id, known)
	}
	usageTestSubscriptionOwner(t, host, "new-public-owner")
	if id, known := CurrentAccountIDs(t.Context())(p); id != "" || known {
		t.Errorf("replaced ownership: ID %q, known %t", id, known)
	}
}
