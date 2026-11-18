package extension

import (
	"encoding/json"
	"testing"
)

func TestProviderDefaultsRequireAPI9(t *testing.T) {
	for _, field := range []string{"maxConcurrency", "maxAccounts"} {
		t.Run(field, func(t *testing.T) {
			manifest := testManifest(OutboundExecutor)
			manifest.Contract.Version.Minor = 8
			descriptor := providerDescriptor()
			if err := json.Unmarshal([]byte(`{"`+field+`":3}`), &descriptor); err != nil {
				t.Fatal(err)
			}
			manifest.Providers = []ProviderDescriptor{descriptor}
			if err := manifest.Validate(); err == nil {
				t.Fatal("provider default silently accepted before API9")
			}
		})
	}
}

func TestProviderDefaultsBoundsAndClone(t *testing.T) {
	for _, value := range []int{0, 1, 1000, -1, 1001} {
		manifest := testManifest(OutboundExecutor)
		manifest.Contract.Version.Minor = 9
		descriptor := providerDescriptor()
		raw, _ := json.Marshal(map[string]int{"maxConcurrency": value, "maxAccounts": value})
		if err := json.Unmarshal(raw, &descriptor); err != nil {
			t.Fatal(err)
		}
		manifest.Providers = []ProviderDescriptor{descriptor}
		if err := manifest.Validate(); (err == nil) != (value >= 0 && value <= 1000) {
			t.Fatalf("default %d: %v", value, err)
		}
		if value < 0 || value > 1000 {
			continue
		}
		copy := manifest.Clone()
		if err := json.Unmarshal([]byte(`{"maxConcurrency":7,"maxAccounts":8}`), &manifest.Providers[0]); err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(copy.Providers[0])
		var saved map[string]any
		json.Unmarshal(wire, &saved)
		if saved["maxConcurrency"] != float64(value) || saved["maxAccounts"] != float64(value) {
			t.Fatalf("cloned defaults lost presence or shared pointers: %s", wire)
		}
	}
}
