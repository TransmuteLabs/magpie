package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

func isTitleKind(kind string) bool {
	return kind == "thread_title" || kind == "thread_title_reconsideration" || kind == "title_generation"
}

// isCommitKind: the Codex app's commit message, pull request text and
// both at once (commit_message, pull_request_message,
// commit_pull_request_message), each asked for in a schema of its own
func isCommitKind(kind string) bool {
	return kind == "commit_message" || kind == "pull_request_message" || kind == "commit_pull_request_message"
}

func isDescriptionKind(kind string) bool {
	return kind == "thread_description"
}

// isSummaryKind: the Codex app's one-line summary of a thread's latest
// message, shown under its title (thread_summary)
func isSummaryKind(kind string) bool {
	return kind == "thread_summary"
}

// isPredictionKind: the Codex app's guess at the user's next message,
// shown in its composer (composer_predictions, #1970)
func isPredictionKind(kind string) bool {
	return kind == "composer_predictions"
}

// Codex projects turn metadata into headers, but its canonical transport is
// client_metadata in the Responses body. Read only identity fields; never keep
// prompts or the rest of the metadata in the trace.
type sessionMetadata struct {
	Turn         string `json:"turn_id"`
	Thread       string `json:"thread_id"`
	Source       string `json:"thread_source"`
	Parent       string `json:"parent_thread_id"`
	Forked       string `json:"forked_from_thread_id"`
	Installation string `json:"installation_id"`
}

func requestSessionMetadata(h http.Header, body []byte) sessionMetadata {
	return sessionMetadataOf(h, requestClientMetadata(body))
}

func requestSessionIdentity(agent string, h http.Header, body []byte) (sessionMetadata, string) {
	metadata := requestClientMetadata(body)
	return sessionMetadataOf(h, metadata), codexPluginSubagent(agent, h, func() map[string]json.RawMessage {
		return metadata
	})
}

func sessionMetadataOf(h http.Header, metadata map[string]json.RawMessage) sessionMetadata {
	var m sessionMetadata
	if raw := metadata["x-codex-turn-metadata"]; len(raw) > 0 {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			raw = []byte(text)
		}
		if json.Unmarshal(raw, &m) == nil {
			return m
		}
	} else if len(metadata) > 0 {
		_ = json.Unmarshal(metadata["turn_id"], &m.Turn)
		_ = json.Unmarshal(metadata["thread_id"], &m.Thread)
		// Compatibility projections used by clients without the full blob.
		_ = json.Unmarshal(metadata["thread_source"], &m.Source)
		_ = json.Unmarshal(metadata["parent_thread_id"], &m.Parent)
		if m.Parent == "" {
			_ = json.Unmarshal(metadata["x-codex-parent-thread-id"], &m.Parent)
		}
		_ = json.Unmarshal(metadata["forked_from_thread_id"], &m.Forked)
		_ = json.Unmarshal(metadata["installation_id"], &m.Installation)
		if m.Source != "" || m.Parent != "" || m.Forked != "" {
			return m
		}
	}
	projectedTurn := m.Turn
	m = sessionMetadata{}
	if json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &m) != nil {
		return sessionMetadata{Turn: projectedTurn}
	}
	if projectedTurn != "" {
		m.Turn = projectedTurn
	}
	return m
}

// requestClientMetadata decodes only client_metadata. For long bodies
// topMember checks and splits the conversation once (#3394).
func requestClientMetadata(body []byte) map[string]json.RawMessage {
	if len(body) < validMemoMin && !bodyHas(body, "client_metadata") {
		return nil
	}
	raw, found, ok := topMember(body, "client_metadata")
	if !ok || !found {
		return nil
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return nil
	}
	return metadata
}

func requestCallKind(h http.Header, m sessionMetadata) string {
	if h.Get("x-openai-subagent") != "" || h.Get("x-openai-memgen-request") != "" {
		return callKind(h)
	}
	switch source := strings.TrimSpace(m.Source); source {
	case "", "user", "subagent":
		return callKind(h)
	default:
		if len(source) > 40 {
			source = source[:40]
		}
		return source
	}
}

// Only title helpers inherit their originating chat. A user-created fork is a
// separate conversation, even though it carries the same fork ancestry.
func titleParentSession(h http.Header, m sessionMetadata, kind string) string {
	if !isTitleKind(kind) {
		return ""
	}
	for _, id := range []string{m.Parent, m.Forked, h.Get("x-codex-parent-thread-id")} {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "\r\n\t") || id == sessionOf(h) {
			continue
		}
		return id
	}
	return ""
}
