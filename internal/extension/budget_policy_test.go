package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

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
