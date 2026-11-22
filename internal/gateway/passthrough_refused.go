package gateway

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/lazyre"
	"github.com/yetone/magpie/internal/provider"
)

// Codex puts its own bookkeeping on each input item it sends OpenAI, in
// internal_chat_message_metadata_passthrough: the turn, when it was made,
// what kinds of content it holds. A newer Codex app adds a member to it
// before the ChatGPT backend takes it for every request: xuanwo on X got
// 400 "[ObjectParam] [input[55].internal_chat_message_metadata_passthrough.
// content_token_count] [unknown_parameter]" on every new conversation
// with two agents, a member no open-source Codex sends (openai/codex#52313
// was the same with content_item_metadata in 0.163.0-alpha.1). The member
// is Codex's attribution, not the conversation, so a request turned away
// over it is asked once more without it on every item, and the provider
// isn't sent it again.

const passthroughKey = "internal_chat_message_metadata_passthrough"

// passthroughRefusal is OpenAI's refusal of one member of the passthrough,
// or of the passthrough itself, as its path in the request: the member's
// name, empty for the whole passthrough.
var passthroughRefusal = lazyre.New(`input\[\d+\]\.` + passthroughKey + `(?:\.([A-Za-z0-9_]+))?\]?\s*\[unknown_parameter\]|(?i:unknown parameter):?\s*'input\[\d+\]\.` + passthroughKey + `(?:\.([A-Za-z0-9_]+))?'`)

// refusedPassthrough is the member msg says the upstream doesn't know,
// "" for the passthrough as a whole.
func refusedPassthrough(msg []byte) (field string, ok bool) {
	m := passthroughRefusal.FindSubmatch(wordsOf(msg))
	if m == nil {
		return "", false
	}
	if len(m[1]) > 0 {
		return string(m[1]), true
	}
	return string(m[2]), true
}

// passthroughRefused is how unfit remembers an upstream turning field
// away; "" is the passthrough as a whole.
func passthroughRefused(field string) string {
	if field == "" {
		return "input[]." + passthroughKey
	}
	return "input[]." + passthroughKey + "." + field
}

// withoutPassthrough is a Responses request with no item's passthrough
// holding field, or with no passthrough at all for "", every other byte as
// it was; false when no item had it.
func withoutPassthrough(body []byte, field string) ([]byte, bool) {
	return withoutPassthroughIf(body, func(f string) bool { return f == field })
}

// withoutPassthroughIf drops each passthrough member drop says, and the
// whole passthrough where drop("") does.
func withoutPassthroughIf(body []byte, drop func(field string) bool) ([]byte, bool) {
	if !gjson.ValidBytes(body) {
		return body, false
	}
	input := getIn(body, "input")
	if !input.IsArray() {
		return body, false
	}
	var out, fields []byte
	last := 0
	input.ForEach(func(_ gjson.Result, item gjson.Result) bool {
		if !item.IsObject() || item.Index <= 0 {
			return true
		}
		metadata := item.Get(passthroughKey)
		if !metadata.Exists() || metadata.Index <= 0 {
			return true
		}
		section := metadata
		var replacement []byte
		if drop("") {
			var object map[string]json.RawMessage
			if json.Unmarshal([]byte(item.Raw), &object) != nil {
				return true
			}
			delete(object, passthroughKey)
			encoded, err := json.Marshal(object)
			if err != nil {
				return true
			}
			section, replacement = item, encoded
		} else {
			if !metadata.IsObject() {
				return true
			}
			fields = append(fields[:0], '{')
			changed := false
			metadata.ForEach(func(key, value gjson.Result) bool {
				if field := key.String(); field != "" && drop(field) {
					changed = true
					return true
				}
				if len(fields) > 1 {
					fields = append(fields, ',')
				}
				fields = append(fields, key.Raw...)
				fields = append(fields, ':')
				fields = append(fields, value.Raw...)
				return true
			})
			if !changed {
				return true
			}
			fields = append(fields, '}')
			replacement = fields
		}
		if out == nil {
			out = make([]byte, 0, len(body))
		}
		out = append(out, body[last:section.Index]...)
		out = append(out, replacement...)
		last = section.Index + len(section.Raw)
		return true
	})
	if out == nil {
		return body, false
	}
	return append(out, body[last:]...), true
}

// withoutRefusedPassthrough is body without the passthrough members
// providerID turned away before.
func (s *Server) withoutRefusedPassthrough(providerID string, proto provider.Protocol, body []byte) []byte {
	if proto != provider.Responses || !bodyHas(body, passthroughKey) {
		return body
	}
	s.mu.Lock()
	empty := len(s.unfit) == 0
	s.mu.Unlock()
	if empty {
		return body
	}
	if !s.fits(providerID, passthroughRefused(""), proto) {
		b, _ := withoutPassthrough(body, "")
		return b
	}
	b, _ := withoutPassthroughIf(body, func(f string) bool {
		return f != "" && !s.fits(providerID, passthroughRefused(f), proto)
	})
	return b
}
