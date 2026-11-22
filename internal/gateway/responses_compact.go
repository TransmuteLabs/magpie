package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/codexsummary"
	"github.com/yetone/magpie/internal/provider"
)

type unaryCompactKey struct{}

func (s *Server) responsesCompact(w http.ResponseWriter, r *http.Request) {
	body, ok := s.requestBody(w, r, provider.Responses)
	if !ok {
		return
	}
	body, model, err := requestModel(body)
	if err != nil {
		writeError(w, provider.Responses, http.StatusBadRequest, err.Error())
		return
	}
	noteAsked(r.Context(), model)
	if !hasMagpieCheckpoint(body) && s.nativeCompactionForWire(model, s.carriesCompaction) {
		r = r.WithContext(context.WithValue(r.Context(), unaryCompactKey{}, true))
		body = keyTiered(r, codexTiered(r, fastTier(body), model), model)
		s.serveAgent(w, r, provider.Responses, body)
		return
	}
	history := compactHistory(body)
	if len(history) == 0 {
		writeError(w, provider.Responses, http.StatusBadRequest, "compact requires nonempty, self-contained input without conversation references, control items or unreadable checkpoints")
		return
	}
	history = append(history, json.RawMessage(`{"type":"compaction_trigger"}`))
	body = withoutFields(body, "text", "context_management")
	body = withFields(body, map[string]any{"input": history, "stream": true, "store": false})
	body, compact := codexInputAs(body, true, false)
	if !compact {
		writeError(w, provider.Responses, http.StatusBadRequest, "could not prepare compaction input")
		return
	}
	body = keyTiered(r, codexTiered(r, fastTier(body), model), model)
	s.compactConversation(w, r, body, true)
}

func compactHistory(body []byte) []json.RawMessage {
	body = bytes.TrimSpace(body)
	for _, field := range []string{"previous_response_id", "conversation"} {
		if raw := memberOrNil(body, field); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			return nil
		}
	}
	input := memberOrNil(body, "input")
	if text, ok := stringBytes(input); ok {
		if strings.TrimSpace(string(text)) == "" {
			return nil
		}
		message, _ := json.Marshal(userMessage(string(text)))
		return []json.RawMessage{message}
	}
	var history []json.RawMessage
	if json.Unmarshal(input, &history) != nil {
		return nil
	}
	for _, item := range history {
		if item[0] != '{' {
			return nil
		}
		kind, _ := stringBytes(memberOrNil(item, "type"))
		switch string(kind) {
		case "item_reference", "configuration_update", additionalToolsItem, "compaction_trigger":
			return nil
		case "compaction", "compaction_summary":
			encrypted, _ := stringBytes(memberOrNil(item, "encrypted_content"))
			summary, ours, ok := codexsummary.Summary(string(encrypted))
			if !ours || !ok || !utf8.ValidString(summary) || strings.TrimSpace(summary) == "" {
				return nil
			}
		}
	}
	return history
}

func hasMagpieCheckpoint(body []byte) bool {
	input := memberOrNil(bytes.TrimSpace(body), "input")
	if len(input) == 0 || input[0] != '[' {
		return false
	}
	found := false
	eachElement(input, func(item []byte) bool {
		kind, _ := stringBytes(memberOrNil(item, "type"))
		if slices.Contains(compactionKinds, string(kind)) {
			seal, _ := stringBytes(memberOrNil(item, "encrypted_content"))
			found = bytes.HasPrefix(seal, []byte(magpieCompaction))
		}
		return !found
	})
	return found
}

// unaryCompact is whether r is a /responses/compact request.
func unaryCompact(r *http.Request) bool {
	v, _ := r.Context().Value(unaryCompactKey{}).(bool)
	return v
}

func compactForAccount(body []byte) ([]byte, []json.RawMessage) {
	input := memberOrNil(bytes.TrimSpace(body), "input")
	var items, kept []json.RawMessage
	if text, ok := stringBytes(input); ok {
		item, _ := json.Marshal(userMessage(string(text)))
		items = append(items, item)
		kept = append(kept, item)
	} else if len(input) > 0 && input[0] == '[' {
		eachElement(input, func(raw []byte) bool {
			item := json.RawMessage(raw)
			items = append(items, item)
			role, _ := stringBytes(memberOrNil(item, "role"))
			if string(role) != "user" {
				return true
			}
			rawKind := memberOrNil(item, "type")
			kind, _ := stringBytes(rawKind)
			if rawKind == nil || string(kind) == "message" {
				kept = append(kept, item)
			}
			return true
		})
	}
	items = append(items, json.RawMessage(`{"type":"compaction_trigger"}`))
	return withFields(body, map[string]any{"input": items, "stream": true}), kept
}

func compactionReply(response json.RawMessage, kept []json.RawMessage, streamed map[int]json.RawMessage) ([]byte, bool) {
	if !jsonCheck(response) {
		return nil, false
	}
	rawOutput := memberOrNil(bytes.TrimSpace(response), "output")
	if len(rawOutput) == 0 || rawOutput[0] != '[' {
		return nil, false
	}
	if len(streamed) > 0 && len(bytes.TrimSpace(rawOutput[1:len(rawOutput)-1])) == 0 {
		response = withStreamedOutput(response, streamed)
		rawOutput = memberOrNil(response, "output")
	}
	result := gjson.ParseBytes(response)
	output := slices.Clone(kept)
	eachElement(rawOutput, func(item []byte) bool {
		kind, _ := stringBytes(memberOrNil(item, "type"))
		if slices.Contains(compactionKinds, string(kind)) {
			if encrypted, _ := stringBytes(memberOrNil(item, "encrypted_content")); len(encrypted) > 0 {
				output = append(output, json.RawMessage(item))
			}
		}
		return true
	})
	if len(output) == len(kept) {
		return nil, false
	}
	id := result.Get("id").Str
	if id == "" {
		id = "cmp_" + newID()
	}
	reply := map[string]any{"id": id, "object": "response.compaction", "output": output}
	if created := result.Get("created_at"); created.Exists() {
		reply["created_at"] = json.RawMessage(created.Raw)
	} else {
		reply["created_at"] = time.Now().Unix()
	}
	if usage := result.Get("usage"); usage.IsObject() {
		reply["usage"] = json.RawMessage(usage.Raw)
	}
	data, err := json.Marshal(reply)
	return data, err == nil
}

// unaryCompactError is what a /responses/compact for model is told when
// nothing that serves it has the route.
func unaryCompactError(model string) string {
	return fmt.Sprintf("/responses/compact is not supported for %s: it needs an API-key provider, ChatGPT account or Copilot account that serves the model on Responses", model)
}
