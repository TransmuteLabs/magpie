package extension

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func operationManifest(t *testing.T) Manifest {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal([]byte(`{
		"schema":1,"id":"fixture.media","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":3},"capabilities":["outbound-executor"]},
		"providers":[{"id":"vendor","name":"Vendor","protocols":["chat"],
		"operations":["embeddings","images.generate","audio.speech","videos.create","videos.get","videos.content"],
		"models":[{"id":"vector","kind":"embedding","inputModalities":["text"],"outputModalities":[],
		"temperature":false,"released":"2026-11-01","serviceTiers":["priority"],
		"contextWindow":100,"maxContextWindow":200,"rate":0.5,"rateWas":1}]}]
	}`), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestProviderOperationContractRejectsUnknownAndOldDeclarations(t *testing.T) {
	manifest := operationManifest(t)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"old API", func(value *Manifest) { value.Contract.Version.Minor = 2 }},
		{"unknown operation", func(value *Manifest) { value.Providers[0].Operations = []ProviderOperation{"fetch-anything"} }},
		{"duplicate operation", func(value *Manifest) {
			value.Providers[0].Operations = []ProviderOperation{OperationEmbeddings, OperationEmbeddings}
		}},
		{"missing executor", func(value *Manifest) { value.Contract.Capabilities = []Capability{ModelDiscovery} }},
		{"wrong wire family", func(value *Manifest) { value.Providers[0].Protocols = []protocolext.Protocol{"gemini"} }},
		{"unknown kind", func(value *Manifest) { value.Providers[0].Models[0].Kind = "arbitrary" }},
		{"unknown modality", func(value *Manifest) { value.Providers[0].Models[0].InputModalities = []string{"secret"} }},
		{"duplicate modality", func(value *Manifest) { value.Providers[0].Models[0].OutputModalities = []string{"text", "text"} }},
		{"invalid date", func(value *Manifest) { value.Providers[0].Models[0].Released = "2026-02-30" }},
		{"invalid tier", func(value *Manifest) { value.Providers[0].Models[0].ServiceTiers = []string{"admin"} }},
		{"duplicate tier", func(value *Manifest) { value.Providers[0].Models[0].ServiceTiers = []string{"priority", "priority"} }},
		{"short max context", func(value *Manifest) { value.Providers[0].Models[0].MaxContextWindow = 99 }},
		{"negative rate", func(value *Manifest) { value.Providers[0].Models[0].Rate = -1 }},
		{"infinite rate", func(value *Manifest) { value.Providers[0].Models[0].RateWas = math.Inf(1) }},
		{"undeclared effort", func(value *Manifest) { value.Providers[0].Models[0].DefaultEffort = "high" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := operationManifest(t)
			test.change(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("unsupported operation or metadata was admitted")
			}
		})
	}
}

func TestExtendedModelPublicSnapshotPreservesExplicitEmptyAndIndependentPolicy(t *testing.T) {
	manifest := operationManifest(t)
	snapshot := manifest.Public()
	manifest.Providers[0].Operations[0] = OperationRerank
	model := &manifest.Providers[0].Models[0]
	model.InputModalities[0] = "image"
	model.ServiceTiers[0] = "ultrafast"
	*model.Temperature = true
	wire, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Manifest
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatal(err)
	}
	policy := roundTrip.Providers[0].Models[0]
	if !roundTrip.Providers[0].SupportsOperation(OperationEmbeddings) ||
		!slices.Equal(policy.InputModalities, []string{"text"}) || policy.OutputModalities == nil ||
		!slices.Equal(policy.ServiceTiers, []string{"priority"}) || policy.Temperature == nil || *policy.Temperature {
		t.Fatalf("public snapshot changed its model policy: %s", wire)
	}
}

func TestProviderExecuteChecksOperationBeforeAuthorHandler(t *testing.T) {
	calls := 0
	module := Module{Manifest: operationManifest(t), Handlers: map[string]Handler{
		MethodProviderExecute: func(context.Context, Call, *Stream) (any, error) {
			calls++
			return ExecuteResult{Status: 200}, nil
		},
	}}
	handlers, _, err := module.prepare()
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{
		`{"protocol":"chat","operation":"images.edit"}`,
		`{"protocol":"gemini","operation":"embeddings"}`,
		`{"protocol":"chat","operation":"fetch-anything"}`,
	} {
		if _, err := handlers[MethodProviderExecute](t.Context(), Call{Scope: Scope{Provider: "vendor"}, Params: json.RawMessage(request)}, nil); err == nil {
			t.Fatalf("unnegotiated operation reached author handler: %s", request)
		}
	}
	if calls != 0 {
		t.Fatalf("unnegotiated operations invoked author code %d times", calls)
	}
	if _, err := handlers[MethodProviderExecute](t.Context(), Call{Scope: Scope{Provider: "vendor"},
		Params: json.RawMessage(`{"protocol":"chat","operation":"embeddings"}`)}, nil); err != nil || calls != 1 {
		t.Fatalf("negotiated operation did not reach author: calls=%d %v", calls, err)
	}
}
