package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountTaskDurableIntentCannotReplayAcrossProcessOwners(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	scope.Operation = "account-task-durable"
	storeTestAccount(t, store, scope)
	if err := store.reserveAccountTask(scope, "pinned"); err != nil {
		t.Fatal(err)
	}
	other := NewStore(root)
	if err := other.reserveAccountTask(scope, "pinned"); err == nil {
		t.Fatal("a fresh owner replayed a durable paid intent")
	}
	receipt, err := other.Operation(scope.Plugin, scope.Operation)
	if err != nil || receipt.State != "reserved" || receipt.Scope != scope {
		t.Fatal("fresh owner lost the unresolved reserved receipt", receipt, err)
	}
	if _, err := store.journal(scope, "dispatched", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Journal(scope, "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Journal(scope, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if err := store.finishAccountTask(scope, "pinned", "uncertain"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.journal(scope, "dispatched", true); err == nil {
		t.Fatal("a fresh owner redispatched an accepted uncertain task")
	}
	if err := other.reserveAccountTask(scope, "pinned"); err == nil {
		t.Fatal("uncertainty allowed a second reservation")
	}
}

func TestAccountTaskUnreadableIntentDoesNotBecomeEmptyQueue(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	scope.Operation = "account-task-unreadable"
	storeTestAccount(t, store, scope)
	if err := store.reserveAccountTask(scope, "pinned"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private", scope.Plugin, "account-tasks", storeHash(scope.Operation)+".json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(root).reserveAccountTask(scope, "pinned"); err == nil {
		t.Fatal("unreadable paid task intent was replaced")
	}
}

func TestAccountTaskDeclaredActionCannotBorrowOrdinaryModuleWrite(t *testing.T) {
	manifest := sdk.Manifest{
		Providers: []sdk.ProviderDescriptor{{ID: "vendor",
			AccountTasks: []sdk.AccountTaskDescriptor{{ID: "ping", Action: "paid.ping"}}}},
		Actions: []sdk.ActionDescriptor{{ID: "paid.ping", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	for _, owner := range []string{"", "paid"} {
		t.Run("owner="+owner, func(t *testing.T) {
			manifest := manifest.Clone()
			manifest.Actions[0].Component = owner
			lease := &Lease{worker: &managedWorker{entry: Installed{Manifest: manifest}}}
			scope := storeTestScope()
			scope.Operation, scope.Attempt = "account-task-forged", 1
			paid, err := checkLeasedAccountTask(t.Context(), lease, sdk.MethodModuleCall, scope, sdk.ModuleCallRequest{Action: "paid.ping",
				Input: json.RawMessage(`{"task":"ping","key":"forged","job":"opaque-template","dueAt":1794656400000,"model":"m1"}`)})
			if !paid || err == nil {
				t.Fatal("direct module.call acquired scheduled paid authority", owner)
			}
		})
	}
}
