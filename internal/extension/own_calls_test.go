package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOwnCallAuthorityRejectsRecallInferenceAndChangedConfiguration(t *testing.T) {
	manager, scope := usageManagerVersion(t, 8)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	scope.Operation, scope.Attempt = "own-authority", 1
	if _, err := manager.Store().Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().Journal(scope, "accepted"); err != nil {
		t.Fatal(err)
	}
	recorded := 0
	restore := OnOwnCall(func(actual sdk.Scope, _ sdk.OwnCall) {
		if actual != scope {
			t.Error("callback received another account", actual)
		}
		recorded++
	})
	defer restore()
	base := context.WithValue(t.Context(), hostAuthorityKey{}, hostAuthority{
		manifest: lease.Manifest(), method: sdk.MethodModuleCall,
	})
	write := lease.group.withAuthority(base, sdk.PrimaryComponent, "write")
	raw := json.RawMessage(`{"call":{"kind":"cache_keepalive","model":"model","usage":{"inputTokens":3}}}`)
	for _, ctx := range []context.Context{
		write,
		withOwnCallParent(write, sdk.MethodProviderExecute),
		withOwnCallParent(context.WithValue(write, gatewayReadOnlyKey{}, true), sdk.MethodModuleCall),
		withOwnCallParent(lease.group.withAuthority(base, sdk.PrimaryComponent, "read"), sdk.MethodModuleCall),
	} {
		if _, err := manager.hostCall(ctx, scope, sdk.MethodUsageAppend, raw); err == nil {
			t.Fatal("read, inference or unbound parent appended own-call usage")
		}
	}
	parent := withOwnCallParent(write, sdk.MethodModuleCall)
	if _, err := manager.ownCallAdmission(parent, scope, nil)(parent); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(parent)
	cancel()
	if _, err := manager.hostCall(canceled, scope, sdk.MethodUsageAppend, raw); err == nil {
		t.Fatal("canceled parent appended own usage")
	}
	config := lease.ConfigVersion()
	updated, err := manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, config, json.RawMessage(`{"changed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.hostCall(parent, scope, sdk.MethodUsageAppend, raw); err == nil {
		t.Fatal("old configuration retained own-call authority")
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, updated.Version, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.hostCall(parent, scope, sdk.MethodUsageAppend, raw); err == nil {
		t.Fatal("restoring identical config values revived old callback authority")
	}
	if recorded != 0 {
		t.Fatal("rejected callbacks reached ordinary usage", recorded)
	}
}

func TestOwnCallCatalogWaitUsesParentDeadline(t *testing.T) {
	manager, scope := usageManagerVersion(t, 8)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	scope.Operation, scope.Attempt = "own-catalog-deadline", 1
	for _, state := range []string{"dispatched", "accepted"} {
		if _, err := manager.Store().Journal(scope, state); err != nil {
			t.Fatal(err)
		}
	}
	recorded := 0
	defer OnOwnCall(func(sdk.Scope, sdk.OwnCall) { recorded++ })()
	authority := hostAuthority{manifest: lease.Manifest(), method: sdk.MethodModuleCall}
	parent := withOwnCallParent(lease.group.withAuthority(t.Context(), sdk.PrimaryComponent, "write"), sdk.MethodModuleCall)
	if _, err := manager.ownCallAdmission(parent, scope, nil)(parent); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"call":{"kind":"cache_keepalive","model":"model","usage":{"inputTokens":3}}}`)
	lock, err := os.OpenFile(filepath.Join(manager.catalog.Root, "catalog.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	held, err := filelock.TryLock(lock)
	if err != nil || !held {
		t.Fatalf("hold catalog lock: %v %v", held, err)
	}
	ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.recordOwnCall(ctx, scope, raw, authority, false)
		result <- err
	}()
	select {
	case err := <-result:
		filelock.Unlock(lock)
		if !errors.Is(err, context.DeadlineExceeded) || recorded != 0 {
			t.Fatalf("own-call result = %v, ledger appends = %d", err, recorded)
		}
	case <-time.After(900 * time.Millisecond):
		filelock.Unlock(lock)
		<-result
		t.Fatal("own-call callback waited for catalog lock past its deadline")
	}
	if entry, err := manager.Store().Operation(scope.Plugin, scope.Operation); err != nil || entry.State != "accepted" {
		t.Fatalf("canceling the callback changed work already accepted: %+v, %v", entry, err)
	}
	if _, err := manager.recordOwnCall(parent, scope, raw, authority, false); err != nil || recorded != 1 {
		t.Fatalf("ordinary ledger callback after releasing the lock: %v, appends=%d", err, recorded)
	}
}
