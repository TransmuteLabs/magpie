package gateway

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func legacyClientWholeHistory(fresh []json.RawMessage) bool {
	if len(fresh) < 2 {
		return false
	}
	for _, raw := range fresh[:len(fresh)-1] {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if _, has := item["id"]; !has {
			continue
		}
		var kind string
		json.Unmarshal(item["type"], &kind)
		if kind == "reasoning" || idOptional[kind] {
			return true
		}
	}
	return false
}

func TestClientWholeHistoryLegacy(t *testing.T) {
	const (
		message = `{"type":"message","content":"next"}`
		hit     = `{"type":"reasoning","id":"rs_1"}`
	)
	for _, test := range []struct {
		name  string
		items []string
		want  bool
	}{
		{"empty", nil, false},
		{"single", []string{hit}, false},
		{"last only", []string{message, hit}, false},
		{"early hit", []string{hit, `{`, message}, true},
		{"late hit", []string{`{"id":"required","type":"item_reference"}`, hit, message}, true},
		{"no hit", []string{message, `{"id":"required","type":"item_reference"}`, message}, false},
		{"top-level id only", []string{`{"type":"message","content":{"id":"nested","type":"message"}}`, message}, false},
		{"id in text only", []string{`{"type":"message","content":"{\"id\":\"in text\"}"}`, message}, false},
		{"case sensitive", []string{`{"type":"message","ID":"msg_1"}`, message}, false},
		{"null item", []string{`null`, message}, false},
		{"array item", []string{`[{"id":"msg_1"}]`, message}, false},
		{"duplicate id null last", []string{`{"type":"message","id":"msg_1","id":null}`, message}, true},
		{"duplicate id null first", []string{`{"type":"message","id":null,"id":"msg_1"}`, message}, true},
		{"duplicate type allowed last", []string{`{"id":"msg_1","type":"item_reference","type":"message"}`, message}, true},
		{"duplicate type required last", []string{`{"id":"msg_1","type":"message","type":"item_reference"}`, message}, false},
		{"escaped keys", []string{`{"\u0069d":"msg_1","t\u0079pe":"message"}`, message}, true},
		{"escaped duplicate type", []string{`{"id":"msg_1","type":"reasoning","t\u0079pe":"item_reference"}`, message}, false},
		{"escaped allowed type", []string{`{"id":"rs_1","type":"\u0072easoning"}`, message}, true},
		{"null id", []string{`{"type":"message","id":null}`, message}, true},
		{"object id", []string{`{"type":"message","id":{"anything":"here"}}`, message}, true},
		{"missing type", []string{`{"id":"msg_1"}`, message}, true},
		{"empty type", []string{`{"id":"msg_1","type":""}`, message}, true},
		{"null type", []string{`{"id":"msg_1","type":"item_reference","type":null}`, message}, true},
		{"object type", []string{`{"id":"msg_1","type":{"type":"item_reference"}}`, message}, true},
		{"number type", []string{`{"id":"msg_1","type":7}`, message}, true},
		{"Unicode text and whitespace", []string{" \t" + `{"id":"消息","type":"message","content":"🦜"}` + "\r\n", message}, true},
		{"Unicode type", []string{`{"id":"msg_1","type":"messagé"}`, message}, false},
		{"lone surrogate in type", []string{`{"id":"msg_1","type":"message\ud800"}`, message}, false},
		{"invalid UTF-8 in text", []string{`{"id":"msg_1","type":"message","content":"` + "\xff" + `"}`, message}, true},
		{"invalid UTF-8 in key", []string{`{"i` + "\xff" + `d":"msg_1","type":"message"}`, message}, false},
		{"invalid UTF-8 in type", []string{`{"id":"msg_1","type":"message` + "\xff" + `"}`, message}, false},
		{"invalid UTF-8 outside string", []string{hit + "\xff", message}, false},
		{"invalid escape in value", []string{`{"id":"msg_1","type":"message","extra":"\q"}`, message}, false},
		{"invalid number in value", []string{`{"id":"msg_1","type":"message","extra":[01]}`, message}, false},
		{"truncated object", []string{`{"id":"msg_1","type":"message"`, message}, false},
		{"trailing comma", []string{`{"id":"msg_1","type":"message",}`, message}, false},
		{"trailing JSON", []string{hit + ` {}`, message}, false},
		{"malformed before hit", []string{`{"id":"msg_1",}`, hit, message}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh := make([]json.RawMessage, len(test.items))
			for index, raw := range test.items {
				fresh[index] = json.RawMessage(raw)
			}
			if old := legacyClientWholeHistory(fresh); old != test.want {
				t.Fatalf("legacy decision = %v, expect %v", old, test.want)
			}
			if got := clientWholeHistory(fresh); got != test.want {
				t.Fatalf("decision = %v, expect %v", got, test.want)
			}
		})
	}
}

func FuzzClientWholeHistoryLegacy(f *testing.F) {
	for _, seed := range []string{
		`{"type":"message","id":"msg_1","content":[{"text":"a turn"}]}`,
		`{"\u0069d":null,"type":"item_reference","t\u0079pe":"\u0072easoning"}`,
		`{"type":"message","id":"msg_1","content":"` + "\xff" + `"}`,
		`{"type":"message","id":"msg_1","other":[}`,
		`{}` + `{"type":"message","id":"msg_1"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		fresh := []json.RawMessage{raw, json.RawMessage(`{"type":"message","content":"next"}`)}
		if got, want := clientWholeHistory(fresh), legacyClientWholeHistory(fresh); got != want {
			t.Fatalf("decision = %v, legacy = %v for %q", got, want, raw)
		}
	})
}

func TestClientWholeHistoryScanAllocations(t *testing.T) {
	for _, test := range []struct {
		name string
		item json.RawMessage
	}{
		{"no id", json.RawMessage(`{"type":"message","role":"user","content":"an earlier turn"}`)},
		{"escaped text", json.RawMessage(`{"type":"message","content":"quoted \"id\" in text"}`)},
		{"matching id", json.RawMessage(`{"type":"message","id":"msg_1","content":"remembered"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh := []json.RawMessage{test.item, json.RawMessage(`{"type":"message","content":"next"}`)}
			if allocs := testing.AllocsPerRun(20, func() { clientWholeHistory(fresh) }); allocs != 0 {
				t.Fatalf("scan allocations = %.0f, expect zero", allocs)
			}
		})
	}
}

func BenchmarkClientWholeHistory(b *testing.B) {
	text := strings.Repeat("text from a previous turn. ", 76)
	hit := json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`)
	for _, size := range []int{128, 178} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			for _, fixture := range []struct {
				name  string
				index int
				tail  string
			}{
				{"no_ids", -1, ""},
				{"no_ids_escaped", -1, `\nquoted \"output\"\n`},
				{"late_hit", size - 2, ""},
				{"early_hit", 0, ""},
			} {
				b.Run(fixture.name, func(b *testing.B) {
					fresh := make([]json.RawMessage, size)
					for index := range fresh {
						fresh[index] = json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` +
							strconv.Itoa(index) + " " + text + fixture.tail + `"}]}`)
					}
					if fixture.index >= 0 {
						fresh[fixture.index] = hit
					}
					want := fixture.index >= 0
					for _, implementation := range []struct {
						name string
						scan func([]json.RawMessage) bool
					}{
						{"stdlib", legacyClientWholeHistory},
						{"scan", clientWholeHistory},
					} {
						b.Run(implementation.name, func(b *testing.B) {
							if got := implementation.scan(fresh); got != want {
								b.Fatalf("decision = %v, expect %v", got, want)
							}
							b.ReportAllocs()
							for b.Loop() {
								implementation.scan(fresh)
							}
						})
					}
				})
			}
		})
	}
}
