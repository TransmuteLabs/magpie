package extension

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUsageResultPreservesUnknownWindowsAndPartialFreshness(t *testing.T) {
	const asOf int64 = 1794470400123
	report := UsageResult{
		AsOf: asOf, ReadDeferred: true, Error: "partial reading",
		Windows: []UsageWindow{
			{Name: "known-zero", Used: 0, AsOf: asOf},
			{Name: "unknown", Unknown: true, Error: "window unavailable", AsOf: asOf - 60000},
		},
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		AsOf         int64                        `json:"asOf"`
		ReadDeferred bool                         `json:"readDeferred"`
		Error        string                       `json:"error"`
		Windows      []map[string]json.RawMessage `json:"windows"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.AsOf != asOf || !wire.ReadDeferred || wire.Error != report.Error || len(wire.Windows) != 2 {
		t.Fatalf("partial reading lost its freshness/error: %s", body)
	}
	known, unknown := wire.Windows[0], wire.Windows[1]
	if string(known["used"]) != "0" || known["unknown"] != nil || string(known["asOf"]) != "1794470400123" {
		t.Fatalf("valid zero became unknown: %s", body)
	}
	if string(unknown["used"]) != "0" || string(unknown["unknown"]) != "true" ||
		string(unknown["error"]) != `"window unavailable"` || string(unknown["asOf"]) != "1794470340123" {
		t.Fatalf("unknown window became a fresh zero: %s", body)
	}
	var roundtrip UsageResult
	if err := json.Unmarshal(body, &roundtrip); err != nil || !reflect.DeepEqual(roundtrip, report) {
		t.Fatalf("usage fields changed on roundtrip: %+v (%v)", roundtrip, err)
	}
}

func TestUsageResultLegacyZeroRemainsKnownWithoutInventedFreshness(t *testing.T) {
	var report UsageResult
	if err := json.Unmarshal([]byte(`{"usage":{},"windows":[{"name":"quota","used":0}]}`), &report); err != nil {
		t.Fatal(err)
	}
	if report.AsOf != 0 || report.ReadDeferred || report.Error != "" || len(report.Windows) != 1 ||
		report.Windows[0].Unknown || report.Windows[0].AsOf != 0 || report.Windows[0].Error != "" || report.Windows[0].Used != 0 {
		t.Fatalf("legacy zero changed meaning: %+v", report)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"asOf", "readDeferred", "error"} {
		if wire[field] != nil {
			t.Fatalf("optional freshness was invented: %s", body)
		}
	}
}
