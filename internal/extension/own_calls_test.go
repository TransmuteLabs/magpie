package extension

import (
	"context"
	"encoding/json"
	"testing"

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
	canceled, cancel := context.WithCancel(withOwnCallParent(write, sdk.MethodModuleCall))
	cancel()
	if _, err := manager.hostCall(canceled, scope, sdk.MethodUsageAppend, raw); err == nil {
		t.Fatal("canceled parent appended own usage")
	}
	config := lease.ConfigVersion()
	updated, err := manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, config, json.RawMessage(`{"changed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.hostCall(withOwnCallParent(write, sdk.MethodModuleCall), scope, sdk.MethodUsageAppend, raw); err == nil {
		t.Fatal("old configuration retained own-call authority")
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, updated.Version, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.hostCall(withOwnCallParent(write, sdk.MethodModuleCall), scope, sdk.MethodUsageAppend, raw); err == nil {
		t.Fatal("restoring identical config values revived old callback authority")
	}
	if recorded != 0 {
		t.Fatal("rejected callbacks reached ordinary usage", recorded)
	}
}
