package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

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
