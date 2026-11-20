package extension

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountSuccessorRequiresDurableRefreshReceipt(t *testing.T) {
	store := NewStore(t.TempDir())
	scope := sdk.Scope{Plugin: "fixture", Provider: "vendor", Account: "alice", Generation: 1, Operation: "refresh-fixture", Attempt: 1}
	original := Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"access":"old"}`)}
	if _, err := store.SaveAccount(original, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	result := sdk.AuthResult{ID: scope.Account, Credentials: json.RawMessage(`{"access":"rotated"}`),
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if _, err := store.finishRefresh(scope, &result, "completed"); err != nil {
		t.Fatal(err)
	}
	current, err := store.RefreshedAccount(scope)
	if err != nil || current.Generation != 2 || string(current.Auth) != `{"access":"rotated"}` {
		t.Fatalf("durable refresh successor was unavailable: generation=%d %v", current.Generation, err)
	}
	for _, generation := range []uint64{1, 2} {
		selected := scope
		selected.Generation = generation
		unconfirmed := errors.New("fixture directory sync failed")
		current, err := store.refreshedAccount(t.Context(), selected, func(*os.Root) error { return unconfirmed })
		if !errors.Is(err, unconfirmed) || current.Generation != 0 || len(current.Auth) != 0 {
			t.Fatalf("unconfirmed refresh publication was adopted from generation %d: %+v %v", generation, current, err)
		}
	}
	original.Auth = json.RawMessage(`{"access":"manual"}`)
	if _, err := store.SaveAccount(original, 2); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []uint64{1, 2} {
		stale := scope
		stale.Generation = generation
		if _, err := store.RefreshedAccount(stale); err == nil {
			t.Fatalf("manual replacement was adopted as an owned refresh from generation %d", generation)
		}
	}
}
