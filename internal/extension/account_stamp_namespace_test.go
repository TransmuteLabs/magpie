package extension

import (
	"encoding/json"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountStampProbeIgnoresUnrelatedEmptyNamespaces(t *testing.T) {
	root := t.TempDir()
	before, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	if _, err := store.StateGet(sdk.Scope{Plugin: "fixture.config"}, "module.config"); err != nil {
		t.Fatal(err)
	}
	after, err := accountStampAt(root)
	if err != nil || !after.Equal(before) {
		t.Fatal("a read of absent config changed the account stamp", err)
	}
	if _, err := store.SaveAccount(Account{Plugin: "fixture.account", Provider: "vendor", ID: "alice",
		Auth: json.RawMessage(`{"token":"synthetic"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	known, err := accountStampAt(root)
	if err != nil || known.Equal(before) {
		t.Fatal("actual account creation did not change the account stamp", err)
	}
	if _, err := store.StateGet(sdk.Scope{Plugin: "fixture.other"}, "module.config"); err != nil {
		t.Fatal(err)
	}
	unchanged, err := accountStampAt(root)
	if err != nil || !unchanged.Equal(known) {
		t.Fatal("an unrelated empty namespace changed an existing account stamp", err)
	}
}
