package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusForbidden,
				Headers: map[string][]string{"X-Fixture-Config": {string(module.Config.Values)}}}); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "data", []byte(reply)); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Status: http.StatusForbidden, Outcome: "not-started"}, nil
		},
	}}
	for _, method := range sdk.RequiredMethods(manifest) {
		if module.Handlers[method] == nil {
			module.Handlers[method] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return nil, &sdk.Error{Code: "unavailable", Message: "fixture method not in use"}
			}
		}
	}
	if !manifest.Contract.Has(sdk.InboundCodec) {
		delete(module.Handlers, sdk.MethodInboundDecode)
		delete(module.Handlers, sdk.MethodInboundResponse)
	}
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

func buildDispatchBindingWorker(t *testing.T) (string, string) {
	t.Helper()
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
	return source, entry
}

func TestExtensionProviderRefusesPausedSelection(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "0")
	source, entry := buildDispatchBindingWorker(t)
	manager := extensionhost.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	previous := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return manager }
	t.Cleanup(func() { providerExtensionHost = previous })
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "fixture.pause", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 9},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []protocolext.Protocol{"messages"}}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := manager.Store().SaveAccount(extensionhost.Account{
			Plugin: manifest.ID, Provider: "vendor", ID: name, Auth: json.RawMessage(`{"token":"fixture"}`),
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	providers, err := ExtensionProviders()
	if err != nil || len(providers) != 2 {
		t.Fatalf("discover dispatch fixture: %d selections: %v", len(providers), err)
	}
	const payload = `{"model":"m1"}`
	request := func(selected Provider, body io.Reader) *http.Request {
		return httptest.NewRequestWithContext(t.Context(), http.MethodPost, selected.Anthropic+"/messages", body)
	}
	check := func(selected Provider, req *http.Request, wantPaused bool) {
		t.Helper()
		response, err := selected.Do(http.DefaultClient, req)
		if response != nil {
			defer response.Body.Close()
		}
		if wantPaused {
			var refusal *extensionhost.OutcomeError
			if response != nil || !errors.As(err, &refusal) || refusal.Outcome != "not-started" {
				t.Fatalf("paused account reached its SDK worker: response=%v error=%v", response != nil, err)
			}
		} else if err != nil || response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("active account did not reach its SDK worker: response=%v error=%v", response, err)
		} else if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
	}
	selected := providers[0]
	var pauseErr error
	paused := request(selected, &dispatchSwapReader{Reader: strings.NewReader(payload), swap: func() {
		pauseErr = SetLoginOn(selected.ID, selected.Account.User, false)
	}})
	if err := selected.Sign(paused.Context(), paused, Anthropic, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	check(selected, paused, true)
	if pauseErr != nil {
		t.Fatal(pauseErr)
	}
	check(selected, request(selected, strings.NewReader(payload)), true)
	check(providers[1], request(providers[1], strings.NewReader(payload)), false)
	if err := SetLoginOn(selected.ID, selected.Account.User, true); err != nil {
		t.Fatal(err)
	}
	check(selected, request(selected, strings.NewReader(payload)), false)
}

func TestMessagesDispatchBindingFollowsActualWorker(t *testing.T) {
	source, entry := buildDispatchBindingWorker(t)
	for _, scenario := range []struct {
		name        string
		minor       int
		heldInbound bool
		configOnly  bool
	}{
		{name: "admission", minor: 8},
		{name: "held inbound", minor: 8, heldInbound: true},
		{name: "held inbound config only", minor: 8, heldInbound: true, configOnly: true},
		{name: "exact signed selection", minor: 9},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager := extensionhost.NewManager(t.TempDir())
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := manager.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.dispatch", Version: "1.0.0", Runtime: "exec", Entry: entry,
				Contract:  sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: scenario.minor}, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.InboundCodec}},
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
				providers, err := discoverExtensionModuleContext(t.Context(), manager, installed, heldSubscriptionOwners)
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
			if scenario.heldInbound {
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
				if scenario.configOnly {
					replacement = selection(installed)
					return nil
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
			if scenario.heldInbound {
				if err := swap(); err != nil {
					t.Fatal(err)
				}
				selected = replacement
			} else {
				body.swap = func() { swapErr = swap() }
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, selected.Anthropic+"/messages", body)
			if err := selected.Sign(ctx, req, Anthropic, []byte(`{"model":"m1"}`)); err != nil {
				t.Fatal(err)
			}
			res, err := selected.Do(http.DefaultClient, req)
			if swapErr != nil {
				t.Fatal(swapErr)
			}
			if scenario.minor == 9 {
				if res != nil {
					res.Body.Close()
				}
				var refusal *extensionhost.OutcomeError
				if !errors.As(err, &refusal) || refusal.Outcome != "not-started" || manager.Running(manifest.ID) {
					t.Fatalf("selected revision dispatched after replacement: response=%v error=%v", res != nil, err)
				}
				selected = replacement
				req = httptest.NewRequestWithContext(ctx, http.MethodPost, selected.Anthropic+"/messages", strings.NewReader(`{"model":"m1"}`))
				if err := selected.Sign(ctx, req, Anthropic, []byte(`{"model":"m1"}`)); err != nil {
					t.Fatal(err)
				}
				res, err = selected.Do(http.DefaultClient, req)
			}
			if err != nil {
				t.Fatal(err)
			}
			wire, readErr := io.ReadAll(res.Body)
			closeErr := res.Body.Close()
			wantBody, want := "1.0.1/1", replacement
			if scenario.heldInbound {
				wantBody, want = "1.0.0/0", original
			}
			if res.StatusCode != http.StatusForbidden || string(wire) != wantBody || readErr != nil || closeErr != nil {
				t.Fatalf("actual worker: %d %q (%v, %v), want 403 %q", res.StatusCode, wire, readErr, closeErr, wantBody)
			}
			actual := ExtensionResponseBindingIdentity(res)
			if actual == "" || actual != want.ExtensionBindingIdentity() ||
				scenario.minor == 8 && actual == selected.ExtensionBindingIdentity() {
				t.Fatal("refusal identity does not follow the worker that actually answered")
			}
		})
	}
}

func TestExtensionProviderPinsEffectiveDispatchConfig(t *testing.T) {
	source, entry := buildDispatchBindingWorker(t)
	const initial = `{"mode":"one","safe":true}`
	const changed = `{"mode":"two","safe":true}`
	const reordered = `{"safe":true,"mode":"one"}`
	const payload = `{"model":"m1"}`

	for _, scenario := range []struct {
		name, replacement string
		heldInbound       bool
		settings          []sdk.FeatureSetting
	}{
		{name: "effective replacement", replacement: changed},
		{name: "retained inbound", replacement: changed, heldInbound: true},
		{name: "equal reorder", replacement: reordered},
		{name: "with defaults", replacement: reordered, settings: []sdk.FeatureSetting{
			{Name: "route", Kind: sdk.FeatureSettingString, Title: "Route", Default: "default"},
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager := extensionhost.NewManager(t.TempDir())
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			minor := 9
			if len(scenario.settings) != 0 {
				minor = sdk.ModuleSettingsAPIMinor
			}
			manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.signed-config", Version: "1.0.0", Runtime: "exec", Entry: entry,
				Contract:  sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor}, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.InboundCodec}},
				Inbounds:  []sdk.InboundDescriptor{{ID: "fixture", Path: "/fixture/signed-config"}},
				Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []protocolext.Protocol{"messages"}}},
				Settings:  scenario.settings,
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			installed, err := manager.Install(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.CompareAndSwapModuleConfig(ctx, manifest.ID, "0", json.RawMessage(initial)); err != nil {
				t.Fatal(err)
			}
			selectProvider := func() Provider {
				t.Helper()
				providers, err := discoverExtensionModuleContext(ctx, manager, installed, heldSubscriptionOwners)
				if err != nil || len(providers) != 1 {
					t.Fatalf("discover dispatch fixture: %d selections: %v", len(providers), err)
				}
				return providers[0]
			}
			selected := selectProvider()
			namespace := filepath.Join(manager.Namespace(), "private", manifest.ID)
			paths, err := filepath.Glob(filepath.Join(namespace, "state", "*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("find config state: %v, %v", paths, err)
			}
			path := paths[0]
			replace := func() {
				t.Helper()
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				body := bytes.Replace(raw, []byte(initial), []byte(scenario.replacement), 1)
				if bytes.Equal(raw, body) || len(raw) != len(body) {
					t.Fatal("fixture replacement must change bytes at identical size")
				}
				next := path + ".replacement"
				if err := os.WriteFile(next, body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(next, path); err != nil {
					t.Fatal(err)
				}
				current, err := os.Stat(path)
				if err != nil || os.SameFile(current, info) || !current.ModTime().Equal(info.ModTime()) {
					t.Fatalf("fixture did not replace config at the same timestamp: %v", err)
				}
			}
			signedCtx := ctx
			requestBody := &dispatchSwapReader{Reader: strings.NewReader(payload), swap: replace}
			if scenario.heldInbound {
				signedCtx = extensionhost.WithInboundLease(ctx)
				registry, err := manager.Protocols()
				if err != nil {
					t.Fatal(err)
				}
				adapter, ok := registry.Lookup(manifest.Inbounds[0].Path)
				if !ok {
					t.Fatal("inbound codec missing")
				}
				call := protocolext.Bind(adapter, signedCtx)
				t.Cleanup(func() { _ = call.Reply(httptest.NewRecorder()).Finish(io.EOF) })
				if _, err := call.Decode(signedCtx, nil, nil); err != nil {
					t.Fatal(err)
				}
				replace()
				requestBody.swap = nil
			}
			request := func(ctx context.Context, selected Provider, body io.Reader) *http.Request {
				return httptest.NewRequestWithContext(ctx, http.MethodPost, selected.Anthropic+"/messages", body)
			}
			req := request(signedCtx, selected, requestBody)
			if err := selected.Sign(signedCtx, req, Anthropic, []byte(payload)); err != nil {
				t.Fatal(err)
			}
			response, err := selected.Do(http.DefaultClient, req)
			if scenario.replacement == changed {
				if response != nil {
					_ = response.Body.Close()
				}
				var refusal *extensionhost.OutcomeError
				if response != nil || !errors.As(err, &refusal) || refusal.Outcome != "not-started" {
					t.Errorf("obsolete selection reached the worker: response=%t error=%v", response != nil, err)
				}
				operations, readErr := os.ReadDir(filepath.Join(namespace, "operations"))
				if len(operations) != 0 || readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
					t.Errorf("selection mismatch should precede journal admission: entries=%d error=%v", len(operations), readErr)
				}
				fresh := selectProvider()
				response, err = fresh.Do(http.DefaultClient, request(ctx, fresh, strings.NewReader(payload)))
			}
			if err != nil || response == nil {
				t.Fatalf("current configuration did not execute: response=%t error=%v", response != nil, err)
			}
			wire, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			var config map[string]any
			wantMode := "one"
			if scenario.replacement == changed {
				wantMode = "two"
			}
			configErr := json.Unmarshal([]byte(response.Header.Get("X-Fixture-Config")), &config)
			if response.StatusCode != http.StatusForbidden || string(wire) != "1.0.0/1" ||
				readErr != nil || closeErr != nil || configErr != nil || config["mode"] != wantMode ||
				len(scenario.settings) != 0 && config["route"] != "default" {
				t.Fatalf("worker configuration: %d %q %v (read=%v close=%v config=%v), want mode %q",
					response.StatusCode, wire, config, readErr, closeErr, configErr, wantMode)
			}
		})
	}
}
