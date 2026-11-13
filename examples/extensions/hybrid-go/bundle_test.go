package hybrid

import (
	"bytes"
	"encoding/json"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestPublicBundleDescriptors(t *testing.T) {
	// Validate before loading a trusted C-shared library: init panics in a
	// malformed native example would terminate its host process.
	var source sdk.Manifest
	decoder := json.NewDecoder(bytes.NewReader(declaration))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		t.Fatal(err)
	}
	root := Manifest()
	if err := root.Validate(); err != nil {
		t.Fatal(err)
	}
	service, err := sdk.ServiceManifest(root, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Validate(); err != nil {
		t.Fatal(err)
	}
	if service.ID != root.ID || service.Version != root.Version || len(service.Actions) != 2 {
		t.Fatal("service descriptor lost its root identity/version or owned actions")
	}
}
