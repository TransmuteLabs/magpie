package extension

import (
	"encoding/json"
	"testing"
)

func keepAliveManifest(t *testing.T, policy string) Manifest {
	t.Helper()
	var manifest Manifest
	raw := `{"schema":1,"id":"keepalive.fixture","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":8},"capabilities":["outbound-executor","account-auth"]},
		"providers":[{"id":"vendor","name":"Fixture","protocols":["chat"],"keepAlive":` + policy + `}]}`
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestAccountKeepAliveSurvivesPublicHandshake(t *testing.T) {
	manifest := keepAliveManifest(t, `{"leadMs":120000}`)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []Manifest{manifest.Clone(), manifest.Public()} {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Providers []struct {
				KeepAlive *struct {
					LeadMs int64 `json:"leadMs"`
				} `json:"keepAlive"`
			} `json:"providers"`
		}
		if json.Unmarshal(raw, &value) != nil || len(value.Providers) != 1 ||
			value.Providers[0].KeepAlive == nil || value.Providers[0].KeepAlive.LeadMs != 120000 {
			t.Fatalf("idle auth renewal declaration lost in public/complete snapshot: %s", raw)
		}
	}
}

func TestAccountKeepAliveRejectsUnnegotiatedOrUnsafeLead(t *testing.T) {
	for _, policy := range []string{
		`{"leadMs":0}`, `{"leadMs":59999}`, `{"leadMs":3600001}`, `{"leadMs":-1}`,
		`{"leadMs":1.5}`, `{"leadMs":"60000"}`, `{"leadMs":null}`,
		`{"leadMs":60000,"account":"other"}`, `false`, `[]`,
	} {
		t.Run(policy, func(t *testing.T) {
			raw, _ := json.Marshal(keepAliveManifest(t, `{}`))
			var value map[string]any
			_ = json.Unmarshal(raw, &value)
			var invalid any
			_ = json.Unmarshal([]byte(policy), &invalid)
			value["providers"].([]any)[0].(map[string]any)["keepAlive"] = invalid
			raw, _ = json.Marshal(value)
			var manifest Manifest
			if json.Unmarshal(raw, &manifest) == nil && manifest.Validate() == nil {
				t.Fatal("unsafe idle auth renewal declaration admitted")
			}
		})
	}
	for _, capabilities := range [][]Capability{{OutboundExecutor}, {OutboundExecutor, AccountAuth}} {
		manifest := keepAliveManifest(t, `{}`)
		manifest.Contract.Capabilities = capabilities
		if len(capabilities) == 2 {
			manifest.Contract.Version.Minor = 7
		}
		if manifest.Validate() == nil {
			t.Fatal("idle auth renewal acquired authority without API8/account-auth")
		}
	}
}

func TestAccountKeepAliveAbsentPreservesOlderSDK(t *testing.T) {
	manifest := keepAliveManifest(t, `null`)
	manifest.Contract.Version.Minor = 3
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{`{}`, `{"leadMs":60000}`, `{"leadMs":3600000}`} {
		if err := keepAliveManifest(t, policy).Validate(); err != nil {
			t.Fatalf("valid bounded renewal policy refused: %s: %v", policy, err)
		}
	}
}
