package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestDecisionOperationUsesActualLeaseBeforeJournal(test *testing.T) {
	for _, minor := range []int{5, 6, 7} {
		test.Run(fmt.Sprint(minor), func(test *testing.T) {
			manager, installed := fixtureManager(test)
			source := fixturePackage(test)
			manifest := installed.Manifest.Clone()
			manifest.Version = "2.0.0"
			manifest.Contract = sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor},
				Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.ModelDiscovery}}
			manifest.Inbounds, manifest.Agents, manifest.Sessions, manifest.Tools = nil, nil, nil, nil
			if minor == 7 {
				manifest.Providers[0].Operations = []sdk.ProviderOperation{sdk.OperationSystemOne}
			}
			catalogTestWriteManifest(test, source, manifest)
			if _, err := manager.Install(test.Context(), source); err != nil {
				test.Fatal(err)
			}
			scope := sdk.Scope{Plugin: manifest.ID, Provider: manifest.Providers[0].ID,
				Operation: "leased-decision-gate", Attempt: 1}
			request := sdk.ExecuteRequest{Protocol: "", Operation: sdk.OperationSystemOne, Model: "model"}
			_, err := manager.Call(test.Context(), manifest.ID, sdk.MethodProviderExecute, scope, request, nil)
			if minor < 7 {
				var refused *sdk.Error
				if !errors.As(err, &refused) || refused.Outcome != "not-started" {
					test.Fatalf("minor %d did not refuse System One before dispatch: %v", minor, err)
				}
				if _, err := manager.Store().Operation(manifest.ID, scope.Operation); err == nil {
					test.Fatal("unnegotiated System One reached durable dispatch")
				}
			} else if err != nil {
				test.Fatalf("negotiated System One did not execute: %v", err)
			}
		})
	}
}

func TestProviderOperationUsesLeasedGenerationBeforeJournal(t *testing.T) {
	manager, original := fixtureManager(t)
	source := fixturePackage(t)
	data, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Contract = sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 3}, Capabilities: []sdk.Capability{sdk.OutboundExecutor}}
	manifest.Inbounds, manifest.Agents, manifest.Sessions, manifest.Tools = nil, nil, nil, nil
	manifest.Providers[0].Operations = []sdk.ProviderOperation{sdk.OperationEmbeddings}
	manifest.Version = "2.0.0"
	catalogTestWriteManifest(t, source, manifest)
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	old, err := manager.Acquire(t.Context(), original.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	manifest.Providers[0].Operations = []sdk.ProviderOperation{sdk.OperationRerank}
	manifest.Version = "3.0.0"
	catalogTestWriteManifest(t, source, manifest)
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: manifest.ID, Provider: "provider", Operation: "new-generation-refusal", Attempt: 1}
	request := sdk.ExecuteRequest{Protocol: "chat", Operation: sdk.OperationEmbeddings, Model: "model"}
	if _, err := manager.Call(t.Context(), manifest.ID, sdk.MethodProviderExecute, scope, request, nil); err == nil {
		t.Fatal("replacement worker inherited an operation it did not declare")
	}
	if _, err := manager.Store().Operation(manifest.ID, scope.Operation); err == nil {
		t.Fatal("refused operation reached durable dispatch")
	}
	scope.Operation = "pinned-generation-success"
	if _, err := old.Call(t.Context(), sdk.MethodProviderExecute, scope, request, nil); err != nil {
		t.Fatalf("pinned worker lost its own declared operation after replacement: %v", err)
	}
}
