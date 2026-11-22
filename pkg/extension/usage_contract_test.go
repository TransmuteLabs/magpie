package extension

import (
	"encoding/json"
	"testing"
)

func TestUsage14FloorApplicabilityAndFreshness(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 4}}}
	raw := json.RawMessage(`{"revision":"1","windows":[{"name":"ordinary","used":100,"notModels":["model-OffPeak"]},{"name":"mcp","unknown":true,"aside":true}]}`)
	_, fresh, err := ValidateUsageResult(manifest, raw)
	if err != nil || !fresh {
		t.Fatalf("ordinary fresh plus aside = %v %v", fresh, err)
	}
	manifest.Contract.Version.Minor = 3
	if _, _, err := ValidateUsageResult(manifest, raw); err == nil {
		t.Fatal("old contract silently accepted applicability")
	}
	manifest.Contract.Version.Minor = 4
	for _, raw := range []string{
		`{"revision":"01","windows":[]}`,
		`{"revision":null,"windows":[]}`,
		`{"revision":"","windows":[]}`,
		`{"windows":[{"name":"quota","used":20,"models":["a"],"notModels":["b"]}]}`,
		`{"windows":[{"name":"quota","used":20,"models":["A","a"]}]}`,
		`{"windows":[{"name":"quota","used":101}]}`,
		`{"windows":[{"name":"quota","used":1,"aside":null}]}`,
		`{"windows":[{"name":"quota","used":1,"asOf":-1}]}`,
		`{"asOf":null,"windows":[{"name":"quota","used":1}]}`,
		`{"windows":[{"name":"quota","used":1,"asOf":null}]}`,
		`{"windows":[{"name":"quota","used":1,"resetsAt":null}]}`,
		`null`,
	} {
		if _, _, err := ValidateUsageResult(manifest, json.RawMessage(raw)); err == nil {
			t.Error("invalid usage contract was accepted")
		}
	}
	for _, raw := range []string{
		`{"revision":"1","readDeferred":true,"windows":[{"name":"quota","used":0}]}`,
		`{"revision":"1","windows":[{"name":"quota","used":0,"asOf":1}]}`,
		`{"revision":"1","windows":[{"name":"quota","unknown":true}]}`,
	} {
		if _, fresh, err := ValidateUsageResult(manifest, json.RawMessage(raw)); err != nil || fresh {
			t.Fatalf("non-authoritative acknowledgment = %v %v", fresh, err)
		}
	}
}

func TestUsageTimestampRange(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 4}}}
	const lastMillisecond = 253402300799999
	for _, field := range []struct {
		name string
		set  func(*UsageResult, int64)
	}{
		{"result asOf", func(result *UsageResult, value int64) { result.AsOf = value }},
		{"window asOf", func(result *UsageResult, value int64) { result.Windows[0].AsOf = value }},
		{"window resetsAt", func(result *UsageResult, value int64) { result.Windows[0].ResetsAt = value }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, value := range []int64{-1, 0, lastMillisecond, lastMillisecond + 1} {
				result := UsageResult{Windows: []UsageWindow{{Name: "quota", Used: 12}}}
				field.set(&result, value)
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				_, fresh, err := ValidateUsageResult(manifest, raw)
				inRange := value >= 0 && value <= lastMillisecond
				if (err == nil) != inRange || !inRange && fresh {
					t.Errorf("timestamp %d: fresh=%v err=%v, want valid=%v", value, fresh, err, inRange)
				}
			}
		})
	}
}
