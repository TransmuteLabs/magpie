package extension

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountLabelPublicationIsOptionalAndGenerationBound(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "unavailable"}[blocked], func(t *testing.T) {
			store, flow := authStoreFixture(t)
			readyAuthStore(t, store, flow)
			target := Account{Plugin: flow.Scope.Plugin, Provider: flow.Scope.Provider, ID: "chosen"}
			dir := filepath.Join(store.root, "private", target.Plugin)
			path := filepath.Join(dir, target.labelFile())
			if blocked {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			result := sdk.AuthResult{ID: target.ID, Name: "reader@example.test", Credentials: json.RawMessage(`{"token":"private"}`)}
			receipt, err := store.publishWindowAuth(t.Context(), target.Plugin, flow.Flow, result, nil)
			if err != nil || receipt.Phase != "complete" {
				t.Fatalf("label write affected credential publication: %s %v", receipt.Phase, err)
			}
			account, err := store.GetAccount(target.Plugin, target.Provider, target.ID)
			if err != nil || account.Generation != 1 || !bytes.Equal(account.Auth, result.Credentials) {
				t.Fatal("credential publication missing", err)
			}
			accounts, err := store.Accounts(target.Plugin, target.Provider)
			if err != nil || len(accounts) != 1 {
				t.Fatalf("listed accounts: %d, %v", len(accounts), err)
			}
			if (blocked && accounts[0].Name != "") || (!blocked && accounts[0].Name != result.Name) {
				t.Fatalf("published label = %q", accounts[0].Name)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
			if err != nil || bytes.Contains(raw, []byte(result.Name)) || !bytes.Contains(raw, []byte(`"schema":4`)) {
				t.Fatal("label changed the schema-4 credential journal", err)
			}
			if blocked {
				return
			}
			saved, err := os.ReadFile(path)
			if err != nil || bytes.Contains(saved, []byte("private")) {
				t.Fatal("invalid public label sidecar", err)
			}
			if err := store.DeleteAccount(target.Plugin, target.Provider, target.ID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("deleted account retained its label file", err)
			}
			target.Auth = json.RawMessage(`{}`)
			if _, err := store.SaveAccount(target, 0); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, saved, 0o600); err != nil {
				t.Fatal(err)
			}
			accounts, err = store.Accounts(target.Plugin, target.Provider)
			if err != nil || len(accounts) != 1 || accounts[0].Generation != 3 || accounts[0].Name != "" {
				t.Fatal("stale label attached to a recreated account", err)
			}
		})
	}
}

func TestAccountSaveKeepsTheLastUsableLabel(t *testing.T) {
	store := NewStore(t.TempDir())
	account := Account{
		Plugin: "fixture", Provider: "provider", ID: "account",
		Name: "reader@example.test", Auth: json.RawMessage(`{"token":"before"}`),
	}
	saved, err := store.SaveAccount(account, 0)
	if err != nil {
		t.Fatal(err)
	}
	account.Name = "bad\nlabel"
	account.Auth = json.RawMessage(`{"token":"after"}`)
	if _, err := store.SaveAccount(account, saved.Generation); err != nil {
		t.Fatalf("optional label rejected credential replacement: %v", err)
	}
	accounts, err := store.Accounts(account.Plugin, account.Provider)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("listed accounts: %d, %v", len(accounts), err)
	}
	if current := accounts[0]; current.Name != saved.Name ||
		current.Generation != saved.Generation+1 || !bytes.Equal(current.Auth, account.Auth) {
		t.Fatal("credential replacement lost the last valid account label")
	}
}
