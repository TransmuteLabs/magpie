package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/codexsummary"
	"github.com/yetone/magpie/internal/provider"
)

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
