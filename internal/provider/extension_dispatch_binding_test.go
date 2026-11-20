package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const dispatchBindingWorker = `package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	var manifest sdk.Manifest
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		os.Exit(1)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodInboundDecode: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return sdk.DecodedRequest{Protocol: "messages", Body: json.RawMessage("{\"model\":\"m1\"}")}, nil
		},
		sdk.MethodInboundResponse: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return json.RawMessage("{}"), nil
		},
		sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			module, _ := sdk.ModuleContextFrom(ctx)
			reply := manifest.Version + "/" + module.Config.Version
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusForbidden}); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "data", []byte(reply)); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Status: http.StatusForbidden, Outcome: "not-started"}, nil
		},
	}}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
`

type dispatchSwapReader struct {
	io.Reader
	swap func()
}

func (reader *dispatchSwapReader) Read(buffer []byte) (int, error) {
	if swap := reader.swap; swap != nil {
		reader.swap = nil
		swap()
	}
	return reader.Reader.Read(buffer)
}

func TestMessagesDispatchBindingFollowsActualWorker(t *testing.T) {
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	main := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(main, []byte(dispatchBindingWorker), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o", filepath.Join(source, entry), main)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dispatch fixture: %v\n%s", err, output)
	}
	for _, heldInbound := range []bool{false, true} {
		name := "admission"
		if heldInbound {
			name = "held inbound"
		}
		t.Run(name, func(t *testing.T) {
			manager := extensionhost.NewManager(t.TempDir())
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := manager.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.dispatch", Version: "1.0.0", Runtime: "exec", Entry: entry,
				Contract:  sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.InboundCodec}},
				Inbounds:  []sdk.InboundDescriptor{{ID: "fixture", Path: "/fixture/dispatch"}},
				Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []protocolext.Protocol{"messages"}}}}
			install := func(version string) (extensionhost.Installed, error) {
				manifest.Version = version
				raw, err := json.Marshal(manifest)
				if err != nil {
					return extensionhost.Installed{}, err
				}
				if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
					return extensionhost.Installed{}, err
				}
				return manager.Install(t.Context(), source)
			}
			selection := func(installed extensionhost.Installed) Provider {
				t.Helper()
				providers, err := discoverExtensionModuleContext(t.Context(), manager, installed)
				if err != nil || len(providers) != 1 {
					t.Fatalf("discover dispatch fixture: %d selections: %v", len(providers), err)
				}
				return providers[0]
			}
			installed, err := install("1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			original := selection(installed)
			ctx := t.Context()
			if heldInbound {
				ctx = extensionhost.WithInboundLease(ctx)
				registry, err := manager.Protocols()
				if err != nil {
					t.Fatal(err)
				}
				adapter, ok := registry.Lookup(manifest.Inbounds[0].Path)
				if !ok {
					t.Fatal("inbound codec missing")
				}
				call := protocolext.Bind(adapter, ctx)
				t.Cleanup(func() { _ = call.Reply(httptest.NewRecorder()).Finish(io.EOF) })
				if _, err := call.Decode(ctx, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			var replacement Provider
			swap := func() error {
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"route":"next"}`)); err != nil {
					return err
				}
				entry, err := install("1.0.1")
				if err == nil {
					replacement = selection(entry)
				}
				return err
			}
			selected := original
			body := &dispatchSwapReader{Reader: strings.NewReader(`{"model":"m1"}`)}
			var swapErr error
			if heldInbound {
				if err := swap(); err != nil {
					t.Fatal(err)
				}
				selected = replacement
			} else {
				body.swap = func() { swapErr = swap() }
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, selected.Anthropic+"/messages", body)
			res, err := selected.Do(http.DefaultClient, req)
			if swapErr != nil {
				t.Fatal(swapErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			wire, readErr := io.ReadAll(res.Body)
			closeErr := res.Body.Close()
			wantBody, want := "1.0.1/1", replacement
			if heldInbound {
				wantBody, want = "1.0.0/0", original
			}
			if res.StatusCode != http.StatusForbidden || string(wire) != wantBody || readErr != nil || closeErr != nil {
				t.Fatalf("actual worker: %d %q (%v, %v), want 403 %q", res.StatusCode, wire, readErr, closeErr, wantBody)
			}
			actual := ExtensionResponseBindingIdentity(res)
			if actual == "" || actual != want.ExtensionBindingIdentity() || actual == selected.ExtensionBindingIdentity() {
				t.Fatal("refusal identity does not follow the worker that actually answered")
			}
		})
	}
}
