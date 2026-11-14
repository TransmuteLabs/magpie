package extension

import (
	"math"
	"strings"
	"testing"
)

func TestCatalogProjectionRequiresExplicitNegotiatedPermission(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: FullContract().Version},
		CatalogAccess: &CatalogAccessDescriptor{Scope: "inventory"}}
	if err := ValidateCatalogAccess(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Contract.Version.Minor = 7
	if err := ValidateCatalogAccess(manifest); err == nil {
		t.Fatal("old SDK acquired catalog authority without negotiation")
	}
	manifest.Contract.Version.Minor = 8
	manifest.CatalogAccess.Scope = "selected"
	if err := ValidateCatalogAccess(manifest); err == nil {
		t.Fatal("selected catalog access was granted without account ownership")
	}
	manifest.Contract.Capabilities = []Capability{AccountAuth}
	if err := ValidateCatalogAccess(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.CatalogAccess.Scope = "all-files"
	if err := ValidateCatalogAccess(manifest); err == nil {
		t.Fatal("catalog permission became an arbitrary host read")
	}
}

func TestCatalogProjectionRetainsUnknownAndMeasuredZero(t *testing.T) {
	zero := 0.0
	valid := func() CatalogReadResult {
		return CatalogReadResult{ReadAt: 1, Accounts: []CatalogAccountProjection{
			{Ref: strings.Repeat("a", 64), Provider: "vendor", Kind: "account",
				Models: []CatalogModelProjection{{ID: "vendor/model", ContextWindow: 256000}},
				Quota:  CatalogQuotaProjection{Known: true, Windows: []CatalogWindowProjection{{Name: "daily", Models: []string{"vendor/model"}, Used: &zero}}}},
			{Ref: strings.Repeat("b", 64), Provider: "other", Kind: "key", Models: []CatalogModelProjection{},
				Quota: CatalogQuotaProjection{Known: false}},
		}}
	}
	if err := ValidateCatalogProjection(valid()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CatalogReadResult){
		func(value *CatalogReadResult) { value.ReadAt = 0 },
		func(value *CatalogReadResult) { value.ReadAt = 1 << 53 },
		func(value *CatalogReadResult) { value.Accounts[1].Ref = value.Accounts[0].Ref },
		func(value *CatalogReadResult) { value.Accounts[0].Ref = "raw-account-name" },
		func(value *CatalogReadResult) { value.Accounts[0].Quota.Known = false },
		func(value *CatalogReadResult) { value.Accounts[0].Models[0].ContextWindow = -1 },
		func(value *CatalogReadResult) { value.Accounts[0].Quota.AsOf = 1 << 53 },
		func(value *CatalogReadResult) { value.Accounts[0].Quota.Windows[0].ResetsAt = 1 << 53 },
		func(value *CatalogReadResult) { value.Accounts[0].Quota.Windows[0].Models = nil },
		func(value *CatalogReadResult) {
			invalid := math.NaN()
			value.Accounts[0].Quota.Windows[0].Used = &invalid
		},
	} {
		value := valid()
		mutate(&value)
		if err := ValidateCatalogProjection(value); err == nil {
			t.Fatal("invalid catalog evidence was published", value)
		}
	}
}

func TestCatalogProjectionServiceRetainsOnlyExplicitRootPermission(t *testing.T) {
	root := compositeManifest()
	root.Contract.Version.Minor = 8
	root.CatalogAccess = &CatalogAccessDescriptor{Scope: "inventory"}
	service, err := ServiceManifest(root, "backend")
	if err != nil || service.CatalogAccess == nil || service.CatalogAccess.Scope != "inventory" ||
		service.Contract.Version.Minor != 8 {
		t.Fatal("declared root catalog access did not reach its service", service, err)
	}
	root.CatalogAccess.Scope = "selected"
	if service.CatalogAccess.Scope != "inventory" {
		t.Fatal("service catalog authority shared its root's mutable descriptor")
	}
	root.CatalogAccess = nil
	service, err = ServiceManifest(root, "backend")
	if err != nil || service.CatalogAccess != nil || service.Contract.Version.Minor != 1 {
		t.Fatal("a legacy sidecar acquired undeclared inventory authority", service, err)
	}
}
