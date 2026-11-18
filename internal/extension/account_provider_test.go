package extension

import (
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestAccountOnlyHostAdmissionDoesNotBorrowSiblingInference(t *testing.T) {
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "service.test", Version: "1.0.0", Entry: "worker", Runtime: "exec",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 11}, Capabilities: []sdk.Capability{sdk.AccountAuth, sdk.ModelDiscovery, sdk.OutboundExecutor, sdk.UsageReporter}},
		Providers: []sdk.ProviderDescriptor{
			{ID: "service", Name: "Service"},
			{ID: "inference", Name: "Inference", Protocols: []protocolext.Protocol{protocolext.Chat}},
		}}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: manifest.ID, Provider: "service", Account: "alice", Generation: 1}
	for _, method := range []string{sdk.MethodProviderModels, sdk.MethodProviderExecute} {
		if err := validateMethod(manifest, method, scope); err == nil {
			t.Fatalf("host admitted %s for the service account", method)
		}
		inference := scope
		inference.Provider = "inference"
		if err := validateMethod(manifest, method, inference); err != nil {
			t.Fatal("host refused the inference sibling:", err)
		}
	}
	for _, method := range []string{sdk.MethodAuthRefresh, sdk.MethodProviderUsage} {
		if err := validateMethod(manifest, method, scope); err != nil {
			t.Fatal("host refused service account lifecycle:", err)
		}
	}
	scope.Provider = "undeclared"
	if err := validateMethod(manifest, sdk.MethodAuthRefresh, scope); err == nil {
		t.Fatal("host admitted an undeclared service account")
	}
}
