package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestBudgetPolicyDiscoveryRespectsCancellation(t *testing.T) {
	manager := NewManager(t.TempDir())
	root, err := manager.catalog.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	unlock, err := catalogLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.EvaluateBudget(ctx, "fixture/policy", sdk.MethodBudgetReserve, sdk.BudgetPolicyRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, ErrBudgetPolicyUnavailable) {
			t.Fatalf("canceled lookup returned %v", err)
		}
	case <-time.After(2 * time.Second):
		unlock()
		<-done
		t.Fatal("the canceled budget lookup waited for the catalog lock")
	}
}

func TestBudgetPolicyHostRefusesCallbacksAndBorrowedScope(t *testing.T) {
	manager := NewManager(t.TempDir())
	scope := sdk.Scope{Plugin: "policy"}
	manifest := sdk.Manifest{ID: scope.Plugin}
	for _, method := range []string{sdk.MethodBudgetReserve, sdk.MethodBudgetSpent} {
		ctx := context.WithValue(t.Context(), hostAuthorityKey{}, hostAuthority{manifest: manifest, method: method})
		for _, callback := range []string{sdk.MethodStateCAS, sdk.MethodComponentCall, sdk.MethodUsageAppend, sdk.MethodNetworkProxy} {
			_, err := manager.hostCall(ctx, scope, callback, json.RawMessage(`{}`))
			if err == nil || !strings.Contains(err.Error(), "budget policy cannot call host services") {
				t.Errorf("%s let %s reach host dispatch: %v", method, callback, err)
			}
		}
		borrowed := scope
		borrowed.Provider = "vendor"
		if err := validateCallScope(manifest, method, borrowed); err == nil {
			t.Fatal("budget policy acquired provider authority")
		}
	}
}
