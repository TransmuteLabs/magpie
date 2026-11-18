package extension

import (
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func TestTransactionalRefreshRequiresAPI13(t *testing.T) {
	manifest := Manifest{
		Schema: ManifestSchema, ID: "refresh", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract:      Contract{Version: APIVersion{Major: 1, Minor: 3}, Capabilities: []Capability{AccountAuth, OutboundExecutor}},
		Providers:     []ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{protocolext.Chat}}},
		Compatibility: &Compatibility{HostAPI: "^1.3"},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	old := FullContract()
	old.Version.Minor = 2
	if err := old.Check(manifest.Contract); err == nil {
		t.Fatal("old host admitted a module requiring transactional refresh")
	}
	manifest.Contract.Version.Minor = 2
	if err := manifest.Validate(); err == nil {
		t.Fatal("compatibility requirement exceeded negotiated API")
	}
	manifest.Compatibility = nil
	if err := manifest.Validate(); err != nil {
		t.Fatalf("old account-auth module refused: %v", err)
	}
}
