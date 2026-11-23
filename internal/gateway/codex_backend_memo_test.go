package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/codexsummary"
)

func TestHeldRawObjectView(t *testing.T) {
	padding := strings.Repeat("x", validMemoMin)
	body := []byte(" \t" + `{"input":false,"\u0069nput":["last"],"\uD800":0,"` + string([]byte{0xff}) + `":1,"pad":"` + padding + `"}` + "\r\n")
	var want map[string]json.RawMessage
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	if valid, _ := longBody(bytes.Clone(body)); !valid {
		t.Fatal("could not warm the hash memo")
	}
	ctx, cancel := context.WithCancel(t.Context())
	holdBody(ctx, body)
	release := func() {
		cancel()
		waitHeldReleased(t, body)
	}
	t.Cleanup(release)
	if _, ok := heldRawObjectView(body); ok {
		t.Fatal("borrowed a body with only a hash memo")
	}
	if entry, held := heldLongOf(body); !held || !entry.valid {
		t.Fatal("could not warm the held memo")
	}
	got, ok := heldRawObjectView(body)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatal("borrowed fields differ from encoding/json")
	}
	input := got["input"]
	start := bytes.Index(body, input)
	if start < 0 || &input[0] != &body[start] || cap(input) != len(input) {
		t.Fatal("the field must share the exact input span with its capacity clipped")
	}
	for _, other := range [][]byte{bytes.Clone(body), body[:len(body)-1], body[1:]} {
		if _, ok := heldRawObjectView(other); ok {
			t.Fatal("borrowed a different pointer or length")
		}
	}
	release()
	if _, ok := heldRawObjectView(body); ok {
		t.Fatal("borrowed after the held memo was released")
	}
	if !bytes.Equal(input, want["input"]) {
		t.Fatal("memo removal changed the bytes already read from the body")
	}
}

func TestHeldRawObjectViewRejectsNonobjectMemos(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"invalid", `{"pad":"` + strings.Repeat("x", validMemoMin) + `",]`},
		{"array", `["` + strings.Repeat("x", validMemoMin) + `"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			ctx, cancel := context.WithCancel(t.Context())
			holdBody(ctx, body)
			t.Cleanup(func() {
				cancel()
				waitHeldReleased(t, body)
			})
			if entry, held := heldLongOf(body); !held || entry.valid != json.Valid(body) {
				t.Fatal("fixture did not record the expected validity")
			}
			if _, ok := heldRawObjectView(body); ok {
				t.Fatal("borrowed a map without an exact positive object memo")
			}
		})
	}
}

func TestCodexInputHeldRewriteMatchesOwnedDecode(t *testing.T) {
	checkpoint, err := json.Marshal(map[string]string{
		"type": "compaction_summary", "encrypted_content": codexsummary.Encrypted("Retained checkpoint."),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		items   string
		change  bool
		compact bool
	}{
		{"restore checkpoint", string(checkpoint) + `,{"type":"message","role":"user","extra":{"n":0,"n":1.25e+1},"text":"<\u0061>"}`, true, false},
		{"rewrite trigger", `{"type":"compaction_trigger"}`, true, true},
		{"incompatible item", string(checkpoint) + `,7`, false, false},
		{"duplicate selects no rewrite", `{"type":"message"}`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(" \n" + `{"input":[{"type":"compaction_trigger"}],"\u0069nput":[` + test.items +
				`],"extra":false,"\u0065xtra":1.2300e+02,"pad":"` + strings.Repeat("p", validMemoMin) + `"}` + "\t")
			original := bytes.Clone(body)
			want, wantCompact := codexInputAs(original, true, false)
			changed := !bytes.Equal(want, original)
			if wantCompact != test.compact || changed != test.change {
				t.Fatal("owned rewrite did not take the expected path")
			}
			ctx, cancel := context.WithCancel(t.Context())
			holdBody(ctx, body)
			release := func() {
				cancel()
				waitHeldReleased(t, body)
			}
			t.Cleanup(release)
			if entry, held := heldLongOf(body); !held || !entry.valid || entry.members == nil {
				t.Fatal("fixture did not hold a positive object memo")
			}
			got, compact := codexInputAs(body, true, false)
			if compact != wantCompact || !bytes.Equal(got, want) || !bytes.Equal(body, original) {
				t.Fatal("held rewrite changed the result or modified its request")
			}
			if !test.change && &got[0] != &body[0] {
				t.Fatal("unchanged request did not return its original slice")
			}
			if test.change {
				release()
				clear(body)
				if !bytes.Equal(got, want) {
					t.Fatal("rewritten bytes kept a share of the request")
				}
			}
		})
	}
}
