package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMarshalRawContainerDepth(t *testing.T) {
	for _, depth := range []int{jsonMaxDepth - 1, jsonMaxDepth, jsonMaxDepth + 1} {
		for _, text := range []string{"ordinary", "\u2028"} {
			raw := json.RawMessage(strings.Repeat("[", depth) + `"` + text + `"` + strings.Repeat("]", depth))
			original := bytes.Clone(raw)
			for _, container := range []struct {
				name  string
				value any
			}{
				{"map", map[string]json.RawMessage{"raw": raw}},
				{"slice", []json.RawMessage{raw}},
			} {
				for _, escape := range []bool{false, true} {
					t.Run(fmt.Sprintf("%d/%s/%q/html-%t", depth, container.name, text, escape), func(t *testing.T) {
						var expected bytes.Buffer
						encoder := json.NewEncoder(&expected)
						encoder.SetEscapeHTML(escape)
						expectedErr := encoder.Encode(container.value)
						actual, accepted := marshalRaw(container.value, escape)
						if accepted != (expectedErr == nil) {
							t.Fatalf("accepted=%t; standard encoder error=%v", accepted, expectedErr)
						}
						if accepted && !bytes.Equal(actual, bytes.TrimSuffix(expected.Bytes(), []byte("\n"))) {
							t.Fatal("encoded bytes differ from the standard encoder")
						}
						if !bytes.Equal(raw, original) {
							t.Fatal("encoding changed the raw input")
						}
					})
				}
			}
		}
	}
}

func TestTurnBodyRawContainerDepth(t *testing.T) {
	for _, depth := range []int{jsonMaxDepth - 2, jsonMaxDepth - 1, jsonMaxDepth} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			raw := json.RawMessage(strings.Repeat("[", depth) + `"ordinary"` + strings.Repeat("]", depth))
			for _, inInput := range []bool{false, true} {
				t.Run(fmt.Sprintf("input-%t", inInput), func(t *testing.T) {
					fields := map[string]json.RawMessage{"model": json.RawMessage(`"m"`)}
					var input []json.RawMessage
					expectedValue := map[string]any{"model": json.RawMessage(`"m"`)}
					if inInput {
						input = []json.RawMessage{raw}
					} else {
						fields["metadata"] = raw
						expectedValue["metadata"] = raw
					}
					expectedValue["input"] = input
					expected, expectedErr := json.Marshal(expectedValue)
					actual, actualErr := turnBody(fields, input)
					if (actualErr == nil) != (expectedErr == nil) {
						t.Fatalf("turn body error=%v; standard encoder error=%v", actualErr, expectedErr)
					}
					if actualErr == nil && !bytes.Equal(actual, expected) {
						t.Fatal("turn body differs from the standard encoder")
					}
				})
			}
		})
	}
}
