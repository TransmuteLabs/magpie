package claude

import (
	"bytes"
	"encoding/json"

	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TranscriptRecord(line []byte, messageID string, spokenOnly bool, emit func(bool, sdk.SessionSourcePart) bool) bool {
	if !bytes.Contains(line, []byte(`"type":"user"`)) && !bytes.Contains(line, []byte(`"type":"assistant"`)) {
		return true
	}
	var entry struct {
		Type    string `json:"type"`
		IsMeta  bool   `json:"isMeta"`
		Message struct {
			ID      string                       `json:"id"`
			Model   string                       `json:"model"`
			Content sessionformat.TranscriptText `json:"content"`
		} `json:"message"`
	}
	entry.Message.Content.SpokenOnly = spokenOnly
	if json.Unmarshal(line, &entry) != nil {
		return true
	}
	switch entry.Type {
	case "user":
		if entry.IsMeta {
			return true
		}
	case "assistant":
		if messageID != "" && entry.Message.ID != messageID {
			return true
		}
	default:
		return true
	}
	content := &entry.Message.Content
	var parts []sdk.SessionSourcePart
	if spokenOnly {
		parts = content.SpokenParts(entry.Type)
	} else {
		parts = transcriptParts(content.Raw, entry.Type)
	}
	for _, part := range parts {
		if part.Role == "assistant" && entry.Message.Model != "<synthetic>" {
			part.Model = entry.Message.Model
		}
		if !emit(entry.Type == "user", part) {
			return false
		}
	}
	return true
}

func transcriptParts(raw json.RawMessage, role string) []sdk.SessionSourcePart {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []sdk.SessionSourcePart{sessionformat.TextPart(role, text)}
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var parts []sdk.SessionSourcePart
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, sessionformat.TextPart(role, block.Text))
		case "thinking":
			parts = append(parts, sdk.SessionSourcePart{Role: role, Kind: "thinking", Text: block.Thinking})
		case "tool_use":
			parts = append(parts, sdk.SessionSourcePart{Role: role, Kind: "tool_use", Name: block.Name, Text: sessionformat.Pretty(block.Input)})
		case "tool_result":
			parts = append(parts, sdk.SessionSourcePart{Role: "tool", Kind: "tool_result", Text: sessionformat.ResultText(block.Content)})
		case "image":
			parts = append(parts, sdk.SessionSourcePart{Role: role, Kind: "image", Text: "[image]"})
		}
	}
	return parts
}
