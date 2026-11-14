package extension

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRichQuotaRequiresAPI9(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 8}}}
	for _, field := range []string{`"amount":0`, `"balance":0`, `"display":""`, `"unit":""`, `"span":0`} {
		raw := json.RawMessage(`{"windows":[{"name":"quota","used":0,` + field + `}]}`)
		if _, _, err := ValidateUsageResult(manifest, raw); err == nil {
			t.Errorf("rich quota silently accepted before API9: %s", field)
		}
	}
}

func TestRichQuotaBoundsPresenceAndFreshness(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 9}}}
	for _, field := range []string{
		`"amount":-1`, `"amount":9007199254740992`, `"amount":"0"`,
		`"balance":-9007199254740992`, `"balance":true`,
		`"limit":-1`, `"limit":9007199254740992`,
		`"display":null`, `"display":"line\nbreak"`, `"display":"` + strings.Repeat("a", 513) + `"`,
		`"unit":null`, `"unit":"` + strings.Repeat("界", 22) + `"`,
		`"span":null`, `"span":-1`, `"span":0.5`, `"span":9223372037`,
	} {
		raw := json.RawMessage(`{"windows":[{"name":"quota","used":0,` + field + `}]}`)
		if _, _, err := ValidateUsageResult(manifest, raw); err == nil {
			t.Errorf("invalid rich quota accepted: %s", field)
		}
	}
	for _, field := range []string{`"amount":0`, `"amount":null`, `"balance":0`, `"balance":-1.25`, `"balance":null`,
		`"span":9223372036`, `"display":"vendor units"`, `"unit":"ACU"`} {
		raw := json.RawMessage(`{"windows":[{"name":"quota","used":0,` + field + `}]}`)
		if _, fresh, err := ValidateUsageResult(manifest, raw); err != nil || !fresh {
			t.Errorf("valid measured zero rejected: %s, fresh=%v error=%v", field, fresh, err)
		}
	}
	raw := json.RawMessage(`{"windows":[{"name":"zero","used":0,"amount":0,"balance":0},{"name":"absent","used":0}]}`)
	result, _, err := ValidateUsageResult(manifest, raw)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(result)
	var back struct{ Windows []map[string]any }
	json.Unmarshal(wire, &back)
	if back.Windows[0]["amount"] != float64(0) || back.Windows[0]["balance"] != float64(0) {
		t.Fatalf("exact zero metadata disappeared: %s", wire)
	}
	if _, exists := back.Windows[1]["amount"]; exists {
		t.Fatalf("missing amount invented: %s", wire)
	}
	for _, window := range []string{
		`{"name":"quota","amount":0,"limit":10}`,
		`{"name":"quota","balance":20}`,
		`{"name":"quota","unknown":true,"used":0,"amount":0,"limit":10}`,
		`{"name":"quota","used":0,"amount":0,"asOf":1}`,
		`{"name":"quota","used":0,"amount":0,"aside":true}`,
	} {
		if _, fresh, err := ValidateUsageResult(manifest, json.RawMessage(`{"windows":[`+window+`]}`)); err != nil || fresh {
			t.Errorf("metadata invented a fresh allowance: %s, fresh=%v error=%v", window, fresh, err)
		}
	}
}
