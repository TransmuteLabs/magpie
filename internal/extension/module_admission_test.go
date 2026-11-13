package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func moduleWindowPackage(t *testing.T, version string) string {
	t.Helper()
	source := catalogTestPackage(t, "window-module", version)
	manifest := sdk.Manifest{
		Schema: 1, ID: "window-module", Version: version, Runtime: "exec", Entry: "bin/worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Actions:  []sdk.ActionDescriptor{{ID: "refresh", InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read"}},
		Window:   &sdk.WindowDescriptor{Metadata: "package.json"},
		Bindings: []sdk.BindingDescriptor{{ID: "page.refresh", From: "window:page", Action: "refresh"}},
	}
	catalogTestWriteManifest(t, source, manifest)
	catalogTestWrite(t, filepath.Join(source, "package.json"),
		[]byte(`{"magpie":{"page":{"file":"ui/index.html"},"panels":[{"slot":"agents","file":"ui/panel.html"}]}}`), 0o600)
	catalogTestWrite(t, filepath.Join(source, "ui", "index.html"), []byte("<p>module page</p>"), 0o600)
	catalogTestWrite(t, filepath.Join(source, "ui", "panel.html"), []byte("<p>module panel</p>"), 0o600)
	return source
}

func TestExplicitWindowAdmissionPreservesPreviousPackage(t *testing.T) {
	for _, bad := range []struct {
		name   string
		change func(string)
	}{
		{"invalid JSON", func(source string) {
			catalogTestWrite(t, filepath.Join(source, "package.json"), []byte("{"), 0o600)
		}},
		{"no described parts", func(source string) {
			catalogTestWrite(t, filepath.Join(source, "package.json"), []byte("{}"), 0o600)
		}},
		{"duplicate metadata fields", func(source string) {
			catalogTestWrite(t, filepath.Join(source, "package.json"), []byte(`{"magpie":{},"magpie":{"page":{"file":"ui/index.html"}}}`), 0o600)
		}},
		{"missing HTML", func(source string) {
			catalogTestWrite(t, filepath.Join(source, "package.json"), []byte(`{"magpie":{"page":{"file":"absent.html"}}}`), 0o600)
		}},
		{"binding to nonexistent filtered panel", func(source string) {
			var manifest sdk.Manifest
			root, err := NewCatalog(source).open(false)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err = catalogManifest(root)
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			manifest.Bindings[0].From = "window:panel:1"
			catalogTestWriteManifest(t, source, manifest)
		}},
	} {
		t.Run(bad.name, func(t *testing.T) {
			catalog := NewCatalog(t.TempDir())
			old := catalogTestInstall(t, catalog, moduleWindowPackage(t, "1.0.0"))
			next := moduleWindowPackage(t, "2.0.0")
			bad.change(next)
			started := false
			if _, err := catalog.Install(t.Context(), next, func(context.Context, Installed) error {
				started = true
				return nil
			}); err == nil || started {
				t.Fatalf("invalid explicit window reached backend activation: %v, started=%t", err, started)
			}
			entries, err := catalog.List()
			if err != nil || len(entries) != 1 || entries[0].Revision != old.Revision {
				t.Fatalf("invalid window replaced the installed generation: %+v %v", entries, err)
			}
		})
	}
}

func TestModulePlatformCompatibilityPrecedesBackendActivation(t *testing.T) {
	source := moduleWindowPackage(t, "1.0.0")
	root, err := NewCatalog(source).open(false)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := catalogManifest(root)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	other := "windows/amd64"
	if runtime.GOOS+"/"+runtime.GOARCH == other {
		other = "darwin/arm64"
	}
	manifest.Compatibility = &sdk.Compatibility{HostAPI: "^1.1", Platforms: []string{other}}
	catalogTestWriteManifest(t, source, manifest)
	catalog := NewCatalog(t.TempDir())
	started := false
	if _, err := catalog.Install(t.Context(), source, func(context.Context, Installed) error {
		started = true
		return nil
	}); err == nil || started {
		t.Fatalf("incompatible platform reached activation: %v, started=%t", err, started)
	}
}

func TestSDK10StateNamedModuleConfigKeepsItsOriginalMeaning(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	source := fixturePackage(t)
	root, err := NewCatalog(source).open(false)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := catalogManifest(root)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Contract.Version.Minor = 0
	manifest.Contract.Capabilities = slices.DeleteFunc(manifest.Contract.Capabilities, func(capability sdk.Capability) bool {
		return capability == sdk.ModuleRPC
	})
	catalogTestWriteManifest(t, source, manifest)
	legacy := json.RawMessage(`"opaque legacy state, not a composite configuration"`)
	if _, err := manager.Store().StateCAS(sdk.Scope{Plugin: manifest.ID}, moduleConfigKey, 0, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal("SDK 1.0 state was reinterpreted as a new configuration:", err)
	}
	if result := toolCall(t, manager, sdk.Scope{}, "pid", nil); !bytes.Contains(result, []byte(`"pid"`)) {
		t.Fatalf("SDK 1.0 worker no longer serves: %s", result)
	}
	value, err := manager.Store().StateGet(sdk.Scope{Plugin: manifest.ID}, moduleConfigKey)
	if err != nil || !bytes.Equal(value.Value, legacy) {
		t.Fatalf("SDK 1.0 state was rewritten: %+v %v", value, err)
	}
}
