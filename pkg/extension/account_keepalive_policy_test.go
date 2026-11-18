package extension

import (
	"testing"
	"time"
)

func TestAccountKeepAliveDefaultCloneAndPrivateService(t *testing.T) {
	manifest := keepAliveManifest(t, `{}`)
	if manifest.Providers[0].KeepAlive.Lead() != time.Minute ||
		(*AccountKeepAliveDescriptor)(nil).Lead() != 0 {
		t.Fatal("absent/default idle renewal policy changed")
	}
	for _, snapshot := range []Manifest{manifest.Clone(), manifest.Public()} {
		snapshot.Providers[0].KeepAlive.LeadMs = 120000
		if manifest.Providers[0].KeepAlive.Lead() != time.Minute {
			t.Fatal("public/complete manifest aliases idle renewal policy")
		}
	}
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, ModuleRPC)
	manifest.Services = []ServiceDescriptor{{
		ID: "helper", Runtime: "exec", Entry: "helper", Start: "lazy",
		Contract: ServiceContract{ID: "fixture.helper", Version: 1},
	}}
	service, err := ServiceManifest(manifest, "helper")
	if err != nil || len(service.Providers) != 0 || service.Contract.Has(AccountAuth) {
		t.Fatalf("private service acquired account renewal authority: %+v %v", service, err)
	}
	for _, lead := range []int64{-1, 1, 59999, 3600001, 1<<63 - 1} {
		manifest := keepAliveManifest(t, `{}`)
		manifest.Providers[0].KeepAlive.LeadMs = lead
		if manifest.Validate() == nil {
			t.Fatalf("programmatic unsafe lead admitted: %d", lead)
		}
	}
}
