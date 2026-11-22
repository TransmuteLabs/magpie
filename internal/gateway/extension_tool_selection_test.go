package gateway

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolAccountPreflightAllocations(t *testing.T) {
	extensionToolGatewayFixture(t)
	arguments := json.RawMessage(`{"value":"hello","_magpie":{"provider":"vendor","account":"selected"}}`)
	measure := func() float64 {
		return testing.AllocsPerRun(10, func() {
			ctx, args, err := extensionToolContext(t.Context(), "extension:tool.module", arguments)
			scope := extension.ToolScopeFrom(ctx)
			if err != nil || scope.Provider != "vendor" || scope.Account != "selected" ||
				scope.Generation != 1 || string(args) != `{"value":"hello"}` {
				t.Fatalf("account tool preparation = %+v, %s, %v", scope, args, err)
			}
		})
	}
	small := measure()

	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "unrelated.tools", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
	}
	for index := range 128 {
		manifest.Tools = append(manifest.Tools, sdk.ToolDescriptor{
			Name: fmt.Sprintf("tool-%03d", index), InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	installMetadataTool(t, manifest)

	large := measure()
	t.Logf("account tool preparation allocations: selected module %.0f, with 128 unrelated tools %.0f", small, large)
	if large-small >= 128 {
		t.Fatalf("preflight allocations grew with unselected tool declarations: %.0f -> %.0f", small, large)
	}
}
