package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAccountWorkflowSelectionParity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "sdk", "js", "test", "fixtures", "account-workflow.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Value json.RawMessage `json:"value"`
		Valid bool            `json:"valid"`
	}
	if err := json.Unmarshal(body, &cases); err != nil || len(cases) == 0 {
		t.Fatal("missing shared account workflow cases:", err)
	}
	for _, item := range cases {
		var selection AccountSelection
		err := json.Unmarshal(item.Value, &selection)
		if (err == nil) != item.Valid {
			t.Fatalf("Go/TS account selection disagreement for %s: %v", item.Value, err)
		}
		if err == nil {
			encoded, err := json.Marshal(selection)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip map[string]any
			if err := json.Unmarshal(encoded, &roundTrip); err != nil || roundTrip["expectedGeneration"] != selection.ExpectedGeneration {
				t.Fatal("account generation lost its string representation")
			}
		}
	}
}
