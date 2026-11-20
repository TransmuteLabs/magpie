package sessionformat

import (
	"encoding/json"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type TranscriptText struct {
	SpokenOnly bool
	Raw        json.RawMessage
	text       string
	blocks     []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
}

func (content *TranscriptText) UnmarshalJSON(raw []byte) error {
	if !content.SpokenOnly {
		return content.Raw.UnmarshalJSON(raw)
	}
	content.text, content.blocks = "", nil
	if raw[0] == '[' {
		if json.Unmarshal(raw, &content.blocks) != nil {
			content.blocks = nil
		}
	} else if json.Unmarshal(raw, &content.text) != nil {
		content.text = ""
	}
	return nil
}

func (content *TranscriptText) SpokenParts(role string) []sdk.SessionSourcePart {
	var parts []sdk.SessionSourcePart
	if content.text != "" {
		parts = append(parts, TextPart(role, content.text))
	}
	for _, block := range content.blocks {
		if block.Type == "text" {
			parts = append(parts, TextPart(role, block.Text))
		}
	}
	return parts
}

func TextPart(role, text string) sdk.SessionSourcePart {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "<") || strings.HasPrefix(trimmed, "# AGENTS.md") {
		return sdk.SessionSourcePart{Role: role, Kind: "context", Text: text}
	}
	return sdk.SessionSourcePart{Role: role, Kind: "text", Text: text}
}
