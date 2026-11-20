package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

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
	if s.nativeCompactionForWire(model, s.compactsUnary) && !hasMagpieCheckpoint(body) {
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
	for _, item := range getIn(body, "input").Array() {
		if slices.Contains(compactionKinds, item.Get("type").Str) &&
			strings.HasPrefix(item.Get("encrypted_content").Str, magpieCompaction) {
			return true
		}
	}
	return false
}

// unaryCompact is whether r is a /responses/compact request.
func unaryCompact(r *http.Request) bool {
	v, _ := r.Context().Value(unaryCompactKey{}).(bool)
	return v
}

// compactsUnary is whether model at p can be asked /responses/compact: a
// key's provider (no account) whose wire carries Responses and asks model
// on it.
func (s *Server) compactsUnary(p provider.Provider, model string) bool {
	return p.Account == nil && s.carriesCompaction(p, model)
}

// unaryCompactCarriers keeps of cands those compactsUnary.
func (s *Server) unaryCompactCarriers(cands []candidate, pl planned) ([]candidate, planned, bool) {
	return keepCandidates(cands, pl, func(c candidate) bool { return s.compactsUnary(c.p, c.model) })
}

// unaryCompactError is what a /responses/compact for model is told when
// nothing that serves it has the route.
func unaryCompactError(model string) string {
	return fmt.Sprintf("/responses/compact is not supported for %s: it is relayed only to a provider whose own API is Responses (an API key's provider asking the model on Responses), and none of those serves %s; compact on /responses instead", model, model)
}
