package extension

import (
	"encoding/json"
	"errors"
	"testing"
)

func decisionManifest(test *testing.T) Manifest {
	test.Helper()
	var manifest Manifest
	if err := json.Unmarshal([]byte(`{
		"schema":1,"id":"fixture.decision","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":7},"capabilities":["outbound-executor","model-discovery"]},
		"providers":[{"id":"judge","name":"Judge","protocols":["chat"],"operations":["systemone"],
		"models":[{"id":"reasoner","kind":"chat","protocols":["chat"]},
		{"id":"decision","kind":"decision","protocols":[],"operations":["systemone"]}]}]
	}`), &manifest); err != nil {
		test.Fatal(err)
	}
	return manifest
}

func TestDecisionOperationNegotiatesItsOwnEnvelope(test *testing.T) {
	manifest := decisionManifest(test)
	manifest.Providers[0].Operations = append(manifest.Providers[0].Operations, OperationEmbeddings)
	if err := manifest.Validate(); err != nil {
		test.Fatalf("SDK 1.7 decision operation was refused: %v", err)
	}
	scope := Scope{Provider: "judge"}
	request := json.RawMessage(`{"protocol":"","operation":"systemone","model":"decision"}`)
	if err := ValidateExecuteOperation(manifest, scope, request); err != nil {
		test.Fatalf("negotiated decision envelope was refused: %v", err)
	}
	for _, minor := range []int{5, 6} {
		old := manifest.Clone()
		old.Contract.Version.Minor = minor
		if err := old.Validate(); err == nil {
			test.Fatalf("minor %d admitted decision declaration", minor)
		}
		var typed *Error
		if err := ValidateExecuteOperation(old, scope, request); !errors.As(err, &typed) || typed.Outcome != "not-started" {
			test.Fatalf("minor %d did not refuse before dispatch: %v", minor, err)
		}
	}
	for _, raw := range []string{
		`{"protocol":"chat","operation":"systemone","model":"decision"}`,
		`{"protocol":"","operation":"systemone","model":""}`,
		`{"protocol":"chat","operation":"embeddings","model":"decision"}`,
	} {
		if err := ValidateExecuteOperation(manifest, scope, json.RawMessage(raw)); err == nil {
			test.Fatalf("decision execute borrowed conversational or missing-model authority: %s", raw)
		}
	}
}

func TestDecisionModelWireDefaultsDoNotGrantChatOrImplicitOperations(test *testing.T) {
	raw, _ := json.Marshal(decisionManifest(test))
	for _, field := range []string{"protocols", "operations"} {
		for _, form := range []string{"missing", "null", "empty"} {
			test.Run(field+"/"+form, func(test *testing.T) {
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				descriptor := fields["providers"].([]any)[0].(map[string]any)
				model := descriptor["models"].([]any)[1].(map[string]any)
				delete(model, field)
				switch form {
				case "null":
					model[field] = nil
				case "empty":
					model[field] = []string{}
				}
				wire, _ := json.Marshal(fields)
				var value Manifest
				if err := json.Unmarshal(wire, &value); err != nil {
					test.Fatal(err)
				}
				err := value.Validate()
				if field == "protocols" && err != nil {
					test.Fatalf("absent decision protocols acquired chat inheritance: %v", err)
				}
				if field == "operations" && err == nil {
					test.Fatal("missing decision association acquired provider authority")
				}
			})
		}
	}
	value := decisionManifest(test)
	value.Contract.Capabilities = []Capability{OutboundExecutor}
	if err := value.Validate(); err == nil {
		test.Fatal("decision executor without account discovery was admitted")
	}
	legacy := operationManifest(test)
	if err := ValidateExecuteOperation(legacy, Scope{Provider: "vendor"},
		json.RawMessage(`{"protocol":"chat","operation":"embeddings","model":"vector"}`)); err != nil {
		test.Fatalf("legacy media operation acquired a new model association requirement: %v", err)
	}
}

func TestDecisionModelAssociationSurvivesPublicClone(test *testing.T) {
	manifest := decisionManifest(test)
	snapshot := manifest.Public()
	wire, err := json.Marshal(snapshot)
	if err != nil {
		test.Fatal(err)
	}
	var fields struct {
		Providers []struct {
			Models []struct {
				ID         string              `json:"id"`
				Operations []ProviderOperation `json:"operations"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(wire, &fields); err != nil {
		test.Fatal(err)
	}
	if len(fields.Providers[0].Models[1].Operations) != 1 || fields.Providers[0].Models[1].Operations[0] != "systemone" {
		test.Fatalf("public model lost decision association: %s", wire)
	}
	snapshot.Providers[0].Models[1].Operations[0] = OperationEmbeddings
	if manifest.Providers[0].Models[1].Operations[0] != OperationSystemOne {
		test.Fatal("public model operation mutation changed the private manifest")
	}
}

func TestDecisionModelPolicyRejectsAmbiguousAuthority(test *testing.T) {
	raw, _ := json.Marshal(decisionManifest(test))
	for _, scenario := range []string{"isolated media", "chat operation", "chat protocol", "unknown operation", "duplicate operation", "non-array operation"} {
		test.Run(scenario, func(test *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				test.Fatal(err)
			}
			descriptor := fields["providers"].([]any)[0].(map[string]any)
			model := descriptor["models"].([]any)[1].(map[string]any)
			model["operations"] = []string{"systemone"}
			switch scenario {
			case "isolated media":
				descriptor["protocols"] = []string{}
				descriptor["operations"] = []string{"systemone", "embeddings"}
			case "chat operation":
				model["kind"] = "chat"
			case "chat protocol":
				model["protocols"] = []string{"chat"}
			case "unknown operation":
				model["operations"] = []string{"unknown"}
			case "duplicate operation":
				model["operations"] = []string{"systemone", "systemone"}
			case "non-array operation":
				model["operations"] = "systemone"
			}
			changed, _ := json.Marshal(fields)
			var value Manifest
			if err := json.Unmarshal(changed, &value); err != nil {
				if scenario == "non-array operation" {
					return
				}
				test.Fatal(err)
			}
			if err := value.Validate(); err == nil {
				test.Fatal("ambiguous decision model authority admitted")
			}
		})
	}
}
