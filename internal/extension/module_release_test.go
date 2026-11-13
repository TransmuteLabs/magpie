package extension

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestUniversalReleaseChecksEveryModuleRuntime(t *testing.T) {
	for _, runtimeName := range []string{"exec", "bun"} {
		t.Run(runtimeName, func(t *testing.T) {
			manifest := releaseTestManifest("bun", "1.2.3")
			manifest.Contract = sdk.Contract{
				Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC},
			}
			manifest.Actions = []sdk.ActionDescriptor{{
				ID: "helper.read", Component: "helper", InputSchema: json.RawMessage(`{}`), Effect: "read",
			}}
			manifest.Services = []sdk.ServiceDescriptor{{
				ID: "helper", Runtime: runtimeName, Entry: "bin/helper",
				Contract: sdk.ServiceContract{ID: "fixture", Version: 1}, Methods: []string{"helper.read"}, Start: "lazy",
			}}
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			archive := releaseTestZIP(t, []releaseZIPEntry{
				{name: sdk.ManifestFile, body: body},
				{name: "bin/worker", body: []byte("// portable Bun controller"), mode: 0o600},
				{name: "bin/helper", body: []byte("// helper fixture"), mode: 0o700},
			})
			fixture := releaseNewFixture(t, "magpie-plugin_any.zip", archive)
			prepared, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo")
			if runtimeName == "exec" {
				if err == nil || cleanup != nil || prepared.Directory != "" {
					if cleanup != nil {
						cleanup()
					}
					t.Fatal("a Go service was accepted as a universal Bun package")
				}
			} else {
				if err != nil {
					t.Fatal("portable controller and service were rejected:", err)
				}
				cleanup()
			}
		})
	}
}
