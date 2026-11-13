package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func moduleBudgetFixture(t *testing.T) (*Manager, *Lease) {
	t.Helper()
	directory := fixturePackage(t)
	path := filepath.Join(directory, sdk.ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Actions = []sdk.ActionDescriptor{{
		ID: "fixture.input-size", Effect: "read", InputSchema: json.RawMessage(`true`),
	}}
	manifest.Bindings = []sdk.BindingDescriptor{{
		ID: "fixture.local-read", From: sdk.PrimaryComponent, Action: "fixture.input-size",
	}}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	return manager, lease
}

func moduleBudgetInput() json.RawMessage {
	return json.RawMessage(`{"data":"` + strings.Repeat("x", 2<<20) + `"}`)
}

func TestModuleRPCLargeRootInputReachesLeasedReadAction(t *testing.T) {
	_, lease := moduleBudgetFixture(t)
	input := moduleBudgetInput()
	raw, err := lease.Call(context.Background(), sdk.MethodModuleCall, sdk.Scope{},
		sdk.ModuleCallRequest{Action: "fixture.input-size", Input: input}, nil)
	if err != nil {
		t.Fatalf("valid 2 MiB root input was refused: %v", err)
	}
	var result struct {
		Bytes int `json:"bytes"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Bytes != len(input) {
		t.Fatalf("leased action received %d bytes; want %d", result.Bytes, len(input))
	}
}

func TestModuleRPCLargerRootBudgetDoesNotExpandHostCallbacks(t *testing.T) {
	manager, lease := moduleBudgetFixture(t)
	manifest := lease.worker.entry.Manifest
	ctx := context.WithValue(context.Background(), hostAuthorityKey{}, hostAuthority{
		manifest: manifest, method: sdk.MethodToolCall,
	})
	ctx = lease.group.withAuthority(ctx, sdk.PrimaryComponent, "read")
	scope := sdk.Scope{Plugin: manifest.ID}
	small, _ := json.Marshal(sdk.ComponentCallRequest{
		Binding: "fixture.local-read", Input: json.RawMessage(`{}`),
	})
	if _, err := manager.hostCall(ctx, scope, sdk.MethodComponentCall, small); err != nil {
		t.Fatalf("small valid callback failed: %v", err)
	}
	large, _ := json.Marshal(sdk.ComponentCallRequest{
		Binding: "fixture.local-read", Input: moduleBudgetInput(),
	})
	var request sdk.ComponentCallRequest
	if decodeHostParams(large, &request) {
		t.Fatal("root RPC budget was incorrectly applied to host callbacks")
	}
	if _, err := manager.hostCall(ctx, scope, sdk.MethodComponentCall, large); err == nil {
		t.Fatal("oversized scoped host callback was accepted")
	}
}

func TestModuleRPCRootEnvelopeStillRejectsMalformedAndUndeclaredInput(t *testing.T) {
	_, lease := moduleBudgetFixture(t)
	for _, raw := range []string{
		`{"action":"fixture.input-size"}`,
		`{"action":"fixture.input-size","input":{},"unknown":true}`,
		`{"action":"not-declared","input":{}}`,
		`{"action":"fixture.input-size","input":{}} trailing`,
	} {
		if _, err := lease.Call(context.Background(), sdk.MethodModuleCall, sdk.Scope{}, json.RawMessage(raw), nil); err == nil {
			t.Error("malformed or undeclared root input was accepted")
		}
	}
	large, _ := json.Marshal(map[string]any{
		"action": "fixture.input-size", "input": moduleBudgetInput(), "unknown": true,
	})
	if _, err := lease.Call(context.Background(), sdk.MethodModuleCall, sdk.Scope{}, json.RawMessage(large), nil); err == nil {
		t.Error("larger root budget accepted an unknown envelope field")
	}
}
