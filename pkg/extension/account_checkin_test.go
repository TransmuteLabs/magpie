package extension

import (
	"encoding/json"
	"math"
	"testing"
)

func checkinManifest(t *testing.T) Manifest {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal([]byte(`{"schema":1,"id":"checkin.fixture","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":8},"capabilities":["outbound-executor","account-auth","module-rpc"]},
		"actions":[{"id":"daily.claim","effect":"write","inputSchema":{"type":"object"}}],
		"providers":[{"id":"vendor","name":"Fixture","protocols":["chat"],"checkin":{"action":"daily.claim","recheck":true}}],
		"activation":["gateway"]}`), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestAccountCheckinWireAndClone(t *testing.T) {
	manifest := checkinManifest(t)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	copy := manifest.Clone()
	copy.Providers[0].Checkin.Action = "other"
	copy.Activation[0] = "other"
	if manifest.Providers[0].Checkin.Action != "daily.claim" || !manifest.ActivatesGateway() {
		t.Fatal("manifest aliases check-in or activation declaration")
	}
	for _, raw := range []string{`null`, `{}`, `{"outcome":"invented"}`, `{"outcome":"claimed","credit":-1}`,
		`{"outcome":"done","until":"tomorrow"}`, `{"outcome":"done","until":"2026-02-30T03:04:05Z"}`,
		`{"outcome":"done","until":"2026-11-14T3:04:05Z"}`, `{"outcome":"done","until":"2026-11-14T03:04:05+24:00"}`,
		`{"outcome":"done","until":"2026-11-14T03:04:05,1Z"}`, `{"outcome":"done","until":"2026-11-14T03:04:05.1234567890Z"}`,
		`{"outcome":"done","streak":2147483648}`, `{"outcome":"claimed","credentials":{}}`,
		`{"outcome":"rate-limited"}`, `{"outcome":"rate-limited","until":null}`, `{"outcome":"rate-limited","until":""}`,
		`{"outcome":"rate-limited","until":"tomorrow"}`} {
		if _, err := DecodeAccountCheckinResult(json.RawMessage(raw)); err == nil {
			t.Fatalf("unsafe check-in result: %s", raw)
		}
	}
	for _, raw := range []string{`{"outcome":"claimed"}`, `{"outcome":"claimed","credit":0}`, `{"outcome":"done","until":"2027-01-02T03:04:05Z"}`,
		`{"outcome":"rate-limited","until":"2027-01-02T03:04:05.123456789Z"}`, `{"outcome":"failed","until":"2027-01-02T03:04:05Z"}`} {
		if _, err := DecodeAccountCheckinResult(json.RawMessage(raw)); err != nil {
			t.Fatalf("valid check-in result: %s %v", raw, err)
		}
	}
	infinite := math.Inf(1)
	if (AccountCheckinResult{Outcome: "claimed", Credit: &infinite}).Validate() == nil {
		t.Fatal("nonfinite grant admitted")
	}
	zero := 0.0
	result := AccountCheckinResult{Outcome: "claimed", Credit: &zero}
	cloned := result.Clone()
	*cloned.Credit = 7
	if *result.Credit != 0 {
		t.Fatal("grant snapshot aliases explicit zero")
	}
	manifest.Services = []ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: "helper",
		Contract: ServiceContract{ID: "checkin.helper", Version: 1}, Methods: []string{"daily.claim"}, Start: "lazy"}}
	manifest.Actions[0].Component = "helper"
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if manifest.Public().Providers[0].Checkin != nil || len(manifest.Public().Actions) != 0 {
		t.Fatal("public primary check-in references a private service action")
	}
	service, err := ServiceManifest(manifest, "helper")
	if err != nil || len(service.Providers) != 0 || len(service.Activation) != 0 {
		t.Fatalf("private service inherited root check-in or activation: %+v %v", service, err)
	}
}

func TestCheckinAndActivationAdmission(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"old minor", func(value map[string]any) {
			value["contract"].(map[string]any)["version"].(map[string]any)["minor"] = 7
		}},
		{"read action", func(value map[string]any) { value["actions"].([]any)[0].(map[string]any)["effect"] = "read" }},
		{"unknown action", func(value map[string]any) {
			value["providers"].([]any)[0].(map[string]any)["checkin"] = map[string]any{"action": "unknown"}
		}},
		{"unknown activation", func(value map[string]any) { value["activation"] = []string{"background"} }},
		{"duplicate activation", func(value map[string]any) { value["activation"] = []string{"gateway", "gateway"} }},
		{"account access absent", func(value map[string]any) {
			value["contract"].(map[string]any)["capabilities"] = []string{"outbound-executor", "module-rpc"}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			raw, _ := json.Marshal(checkinManifest(t))
			var value map[string]any
			_ = json.Unmarshal(raw, &value)
			if _, exists := value["activation"]; !exists {
				value["activation"] = []string{"gateway"}
			}
			provider := value["providers"].([]any)[0].(map[string]any)
			provider["checkin"] = map[string]any{"action": "daily.claim", "recheck": true}
			change.edit(value)
			raw, _ = json.Marshal(value)
			var manifest Manifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				return
			}
			if err := manifest.Validate(); err == nil {
				t.Fatal("unsafe check-in/activation declaration admitted")
			}
		})
	}
}
