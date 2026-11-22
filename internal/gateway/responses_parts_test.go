package gateway

import (
	"bytes"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestResponsesPartsChecksMalformedArrayOnce(t *testing.T) {
	raw := []byte(`[{"type":"input_text","text":"` + strings.Repeat("x", validMemoMin) + `"}`)
	var scans atomic.Int32
	observe := func(data []byte) {
		if bytes.Equal(data, raw) {
			scans.Add(1)
		}
	}
	previous := validScanned.Swap(&observe)
	t.Cleanup(func() { validScanned.Store(previous) })
	if parts := responsesParts(raw); len(parts) != 0 {
		t.Fatalf("truncated content returned %d parts", len(parts))
	}
	if got := scans.Load(); got != 1 {
		t.Fatalf("truncated content had %d validation scans, want one", got)
	}
}

func TestResponsesPartsAcceptsTheSameContentShapes(t *testing.T) {
	for _, fixture := range []struct {
		name, raw string
		want      []Part
	}{
		{name: "array with whitespace", raw: " \t\r\n" + `[{"type":"input_text","text":"words\u003c"}]`,
			want: []Part{{Kind: Text, Text: "words<"}}},
		{name: "scalar string", raw: " \t\r\n" + `"words\u003c"`, want: []Part{{Kind: Text, Text: "words<"}}},
		{name: "empty string", raw: `""`},
		{name: "null", raw: " \t\r\nnull"},
		{name: "omitted"},
		{name: "empty array", raw: `[]`},
		{name: "boolean", raw: `true`},
		{name: "number", raw: `42`},
		{name: "object", raw: `{"type":"input_text","text":"not an array"}`},
		{name: "trailing bytes", raw: `[{"type":"input_text","text":"invalid"}]false`},
		{name: "partially typed array", raw: `[{"type":"input_text","text":false},{"type":"input_text","text":"still present"}]`,
			want: []Part{{Kind: Text}, {Kind: Text, Text: "still present"}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got := responsesParts([]byte(fixture.raw))
			if !reflect.DeepEqual(got, fixture.want) {
				t.Fatalf("parts = %+v, want %+v", got, fixture.want)
			}
		})
	}
}

func TestResponsesPartsArrayOwnsTheTranslatedText(t *testing.T) {
	const message = `{"model":"m","input":[{"role":"user","content":`
	const quoted = `"plain words followed by \u003cquote\u003e and a snowman \u2603"`
	arrayBody := []byte(message + `[{"type":"input_text","text":` + quoted + `}]}]}`)
	array, err := parse(provider.Responses, arrayBody)
	if err != nil {
		t.Fatal(err)
	}
	clear(arrayBody)
	scalar, err := parse(provider.Responses, []byte(message+quoted+`}]}`))
	if err != nil {
		t.Fatal(err)
	}
	arrayWire := buildChat(array, "m", "api.example.com", false)
	scalarWire := buildChat(scalar, "m", "api.example.com", false)
	if !bytes.Equal(arrayWire, scalarWire) {
		t.Fatalf("array wire = %s, scalar wire = %s", arrayWire, scalarWire)
	}
	read, err := parseChat(arrayWire)
	if err != nil {
		t.Fatal(err)
	}
	const want = "plain words followed by <quote> and a snowman ☃"
	if len(read.Messages) != 1 || read.Messages[0].Role != "user" || text(read.Messages[0].Parts) != want {
		t.Fatalf("translated messages = %+v, want one user message with %q", read.Messages, want)
	}
}
