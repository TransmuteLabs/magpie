package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func toolExposureFixture(t *testing.T, version, exposure string) string {
	t.Helper()
	directory := fixturePackage(t)
	data, err := os.ReadFile(filepath.Join(directory, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Version = version
	manifest.Contract.Version.Minor = 2
	manifest.Tools[0].Exposure = exposure
	catalogTestWriteManifest(t, directory, manifest)
	return directory
}

func TestToolExposureAuthorizationUsesTheExecutingGeneration(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(t.Context(), toolExposureFixture(t, "1.0.0", "shared")); err != nil {
		t.Fatal(err)
	}
	// A remote request has completed its discovery/descriptor check. The
	// replacement is published before that request acquires its worker.
	checked, err := manager.Manifest("fixture")
	if err != nil || checked.Tools[0].Exposure != "shared" {
		t.Fatal("shared discovery snapshot was not established", err)
	}
	shared, err := manager.Acquire(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if _, err := manager.Install(t.Context(), toolExposureFixture(t, "2.0.0", "local-owner")); err != nil {
		t.Fatal(err)
	}
	write := sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(
		`{"action":"cas","key":"private.must-not-run","version":"0","value":true}`,
	)}
	if _, err := manager.Call(t.Context(), "fixture", sdk.MethodToolCall, sdk.Scope{}, write, nil); err == nil {
		t.Fatal("a stale shared check authorized execution in the private replacement")
	}
	value, err := manager.Store().StateGet(sdk.Scope{Plugin: "fixture"}, "private.must-not-run")
	if err != nil || value.Version != 0 {
		t.Fatalf("denied replacement executed a write: %+v %v", value, err)
	}
	// An already acquired shared generation retains its own declaration.
	if _, err := shared.Call(t.Context(), sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"get","key":"private.must-not-run"}`)}, nil); err != nil {
		t.Fatalf("the old shared lease was retargeted to the private declaration: %v", err)
	}
	if _, err := manager.Call(WithLocalOwner(t.Context(), true), "fixture", sdk.MethodToolCall,
		sdk.Scope{}, write, nil); err != nil {
		t.Fatalf("the owner could not use the private replacement: %v", err)
	}
}

func TestToolExposureLeaseRejectsUndeclaredNames(t *testing.T) {
	manager, _ := fixtureManager(t)
	if _, err := manager.Call(t.Context(), "fixture", sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "undeclared-alias", Arguments: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("an undeclared alias bypassed the executing manifest")
	}
}
