package extension

import (
	"encoding/json"
	"testing"
)

func TestDecodeMemorySearchUsesExactFields(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"Query":"checkpoint"}`), json.RawMessage(`{"query":"checkpoint","limit":null}`),
		json.RawMessage(`{"query":"checkpoint","Limit":1}`), json.RawMessage(`{"query":"checkpoint","project":"other"}`),
		json.RawMessage(`{"query":"checkpoint","limit":11}`), json.RawMessage(`{"query":" "}`),
	} {
		if _, err := DecodeMemorySearch(input); err == nil {
			t.Errorf("accepted invalid Memory search %s", input)
		}
	}
	query, err := DecodeMemorySearch(json.RawMessage(`{"query":"checkpoint","limit":1}`))
	if err != nil || query != (MemorySearchRequest{Query: "checkpoint", Limit: 1}) {
		t.Fatalf("valid Memory search: %+v %v", query, err)
	}
}
