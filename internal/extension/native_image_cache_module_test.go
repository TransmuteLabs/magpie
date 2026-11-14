//go:build linux && cgo

package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const nativeImageModuleSource = `package main
import (
    "context"
    _ "embed"
    "encoding/json"
    "os"
    "sync"
    sdk "github.com/yetone/magpie/pkg/extension"
    "github.com/yetone/magpie/pkg/extension/native"
)
//go:embed magpie-plugin.json
var declaration []byte
var imageVersion string
type state struct { sync.Mutex; Calls int; Value int }
func main() {}
func init() {
    var manifest sdk.Manifest
    if err := json.Unmarshal(declaration, &manifest); err != nil { panic(err) }
    err := native.Register(sdk.Module{
        Manifest: manifest,
        Start: func(ctx context.Context, module sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
            var config struct { Value int }
            if err := json.Unmarshal(module.Config.Values, &config); err != nil {
                return sdk.ModuleReady{}, nil, err
            }
            return sdk.ModuleReady{State: &state{Value: config.Value}}, nil, nil
        },
        Handlers: map[string]sdk.Handler{
            sdk.MethodToolCall: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
                module, _ := sdk.ModuleContextFrom(ctx)
                value := module.State.(*state)
                value.Lock()
                value.Calls++
                body, err := json.Marshal(map[string]any{
                    "pid":os.Getpid(), "calls":value.Calls, "value":value.Value,
                    "instance":module.Instance, "config":module.Config.Version,
                    "binary":imageVersion,
                })
                value.Unlock()
                if err != nil { return nil, err }
                content, err := json.Marshal([]map[string]string{{"type":"text", "text":string(body)}})
                return sdk.ToolResult{Content:content}, err
            },
        },
    })
    if err != nil { panic(err) }
}`

type nativeImageIdentity struct {
	PID      int    `json:"pid"`
	Calls    int    `json:"calls"`
	Value    int    `json:"value"`
	Instance string `json:"instance"`
	Config   string `json:"config"`
	Binary   string `json:"binary"`
}

func nativeImageBuildStateful(t *testing.T, version string) string {
	t.Helper()
	directory := t.TempDir()
	manifest := sdk.Manifest{
		Schema: 1, ID: "native-image-stateful", Version: version, Runtime: "native", Entry: "plugin.so",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
		Tools:    []sdk.ToolDescriptor{{Name: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, sdk.ManifestFile)
	if err := os.WriteFile(manifestPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "module.go")
	if err := os.WriteFile(source, []byte(nativeImageModuleSource), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{
		filepath.Join(root, "examples/extensions/echo-native/main.go"):           source,
		filepath.Join(root, "examples/extensions/echo-native", sdk.ManifestFile): manifestPath,
	}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	command := proc.CommandContext(t.Context(), "go", "build", "-trimpath", "-overlay", overlayPath,
		"-buildmode=c-shared", "-ldflags", "-X main.imageVersion="+version,
		"-o", filepath.Join(directory, manifest.Entry), "./examples/extensions/echo-native")
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build stateful native image fixture: %v\n%s", err, output)
	}
	return directory
}

func nativeImageReadState(t *testing.T, ctx context.Context, manager *Manager, lease *Lease) nativeImageIdentity {
	t.Helper()
	request := sdk.ToolRequest{ID: "probe", Arguments: json.RawMessage(`{}`)}
	var result sdk.ToolResult
	var err error
	if lease == nil {
		err = manager.Invoke(ctx, "native-image-stateful", sdk.MethodToolCall, sdk.Scope{}, request, &result, nil)
	} else {
		var raw json.RawMessage
		raw, err = lease.Call(ctx, sdk.MethodToolCall, sdk.Scope{}, request, nil)
		if err == nil {
			err = json.Unmarshal(raw, &result)
		}
	}
	if err != nil {
		t.Fatalf("stateful native invocation: %v (cause: %v)", err, errors.Unwrap(err))
	}
	var identity nativeImageIdentity
	nativeToolContent(t, result, &identity)
	if identity.PID != os.Getpid() || identity.Instance == "" {
		t.Fatal("native image changed execution runtime or lost logical instance:", identity)
	}
	return identity
}

func TestNativeImageManagerGenerationAndAuthorityLifecycle(t *testing.T) {
	if os.Getenv("MAGPIE_NATIVE_IMAGE_TEST") != "1" {
		t.Skip("explicit native Manager lifecycle fixture opt-in required")
	}
	before := nativeImageMappings(t)
	v1, v2 := nativeImageBuildStateful(t, "1.0.0"), nativeImageBuildStateful(t, "2.0.0")
	manager := NewManager(filepath.Join(t.TempDir(), "catalog"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	defer managerReleaseClose(t, manager)
	marker := filepath.Join(t.TempDir(), "fallback-ran")
	manager.Bun = filepath.Join(t.TempDir(), "bun")
	if err := os.WriteFile(manager.Bun, []byte("#!/bin/sh\n: > '"+marker+"'\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	archiveV1 := managerReleaseArchive(t, v1)
	manager.ReleaseClient = managerReleaseFixtureClient("1.0.0", archiveV1)
	entry, err := manager.Install(ctx, "github:fixture/module")
	if err != nil {
		t.Fatal(err)
	}
	original := nativeImageReadState(t, ctx, manager, nil)
	held, err := manager.Acquire(ctx, entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	config, err := manager.CompareAndSwapModuleConfig(ctx, entry.Manifest.ID, "0", json.RawMessage(`{"value":7}`))
	if err != nil {
		t.Fatal(err)
	}
	configured := nativeImageReadState(t, ctx, manager, nil)
	prior := nativeImageReadState(t, ctx, manager, held)
	if configured.Value != 7 || configured.Config != config.Version || configured.Calls != 1 ||
		configured.Instance == original.Instance || prior.Instance != original.Instance ||
		prior.Value != 0 || prior.Calls != original.Calls+1 {
		t.Fatal("image reuse crossed configured ModuleReady.State or held lease:", original, configured, prior)
	}
	held.Close()
	if err := manager.Restart(ctx, entry.Manifest.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 || entries[0].Revision == entry.Revision {
		t.Fatal("Restart collapsed logical package generation:", err)
	}
	restarted := nativeImageReadState(t, ctx, manager, nil)
	if restarted.Instance == configured.Instance || restarted.Value != 7 || restarted.Calls != 1 {
		t.Fatal("Restart reused logical State instead of only the image:", restarted)
	}
	updateLease, err := manager.Acquire(ctx, entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer updateLease.Close()
	manager.ReleaseClient = managerReleaseFixtureClient("2.0.0", managerReleaseArchive(t, v2))
	updated, err := manager.Update(ctx, entry.Manifest.ID)
	if err != nil {
		t.Fatal("ordinary GitHub native Update:", err)
	}
	newVersion := nativeImageReadState(t, ctx, manager, nil)
	heldVersion := nativeImageReadState(t, ctx, manager, updateLease)
	if updated.Manifest.Version != "2.0.0" || newVersion.Binary != "2.0.0" || newVersion.Calls != 1 ||
		newVersion.Value != 7 || heldVersion.Binary != "1.0.0" || heldVersion.Instance != restarted.Instance {
		t.Fatal("native update crossed pinned binary/config authority:", newVersion, heldVersion)
	}
	updateLease.Close()
	provenance := *updated.Source
	provenance.Reference = "github:fixture/other"
	rebound, err := manager.install(ctx, v2, installOptions{source: &provenance})
	if err != nil {
		t.Fatal("same bytes with distinct release authority:", err)
	}
	reboundIdentity := nativeImageReadState(t, ctx, manager, nil)
	if rebound.Source.Reference == updated.Source.Reference || reboundIdentity.Binary != "2.0.0" {
		t.Fatal("release source authority was lost")
	}
	installedFile := filepath.Join(manager.root, rebound.Directory, rebound.Manifest.Entry)
	originalBytes, err := os.ReadFile(installedFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedFile, append(originalBytes, []byte("tampered")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Restart(ctx, rebound.Manifest.ID); err == nil {
		t.Fatal("Manager native cache hit bypassed package file verification")
	}
	if err := os.WriteFile(installedFile, originalBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	offLease, err := manager.Acquire(ctx, entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer offLease.Close()
	if err := manager.SetEnabled(ctx, entry.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if admitted, err := manager.Acquire(ctx, entry.Manifest.ID); err == nil {
		admitted.Close()
		t.Fatal("native image cache admitted a disabled module")
	}
	stillHeld := nativeImageReadState(t, ctx, manager, offLease)
	if stillHeld.Instance != reboundIdentity.Instance {
		t.Fatal("disable changed an already pinned native instance")
	}
	offLease.Close()
	if err := manager.SetEnabled(ctx, entry.Manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	reopened := nativeImageReadState(t, ctx, manager, nil)
	if reopened.Instance == stillHeld.Instance || reopened.Calls != 1 || reopened.Value != 7 {
		t.Fatal("off/on reused logical State:", reopened)
	}
	after := nativeImageMappings(t)
	for key := range before {
		delete(after, key)
	}
	if len(after) != 3 {
		t.Fatalf("expected exactly three images for v1, v2 and distinct provenance; mapped %d", len(after))
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native lifecycle silently invoked Bun or executable fallback")
	}
	t.Log("Manager Restart/config/held State/update/source/tamper/off-on preserved; three authority images, no fallback")
}
