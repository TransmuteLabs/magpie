package codex

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type TranscriptDecoder struct {
	model string
}

type transcriptMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (message transcriptMessage) add(emit func(bool, sdk.SessionSourcePart) bool) bool {
	if message.Role != "user" && message.Role != "assistant" {
		return true
	}
	var words []string
	for _, block := range message.Content {
		words = append(words, block.Text)
	}
	return emit(message.Role == "user", sessionformat.TextPart(message.Role, strings.Join(words, "\n")))
}

func (decoder *TranscriptDecoder) Record(line []byte, keep func([]byte) bool, spokenOnly bool, add func(bool, sdk.SessionSourcePart) bool) bool {
	if !bytes.Contains(line, []byte(`"type":"response_item"`)) {
		if bytes.Contains(line, []byte(`"type":"turn_context"`)) {
			var entry struct {
				Type    string `json:"type"`
				Payload struct {
					Model string `json:"model"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &entry) == nil && entry.Type == "turn_context" {
				decoder.model = entry.Payload.Model
			}
		}
		return true
	}
	if keep != nil && !keep(line) {
		return true
	}
	emit := func(input bool, part sdk.SessionSourcePart) bool {
		if part.Role == "assistant" {
			part.Model = decoder.model
		}
		return add(input, part)
	}
	if spokenOnly {
		if gjson.GetBytes(line, "payload.type").Str != "message" {
			return true
		}
		var entry struct {
			Payload transcriptMessage `json:"payload"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Payload.Type != "message" {
			return true
		}
		return entry.Payload.add(emit)
	}
	var entry struct {
		Payload struct {
			transcriptMessage
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
			Summary   []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Action struct {
				Command []string `json:"command"`
			} `json:"action"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return true
	}
	payload := &entry.Payload
	switch payload.Type {
	case "message":
		return payload.transcriptMessage.add(emit)
	case "reasoning", "magpie_reasoning":
		var summary []string
		for _, block := range payload.Summary {
			summary = append(summary, block.Text)
		}
		return emit(false, sdk.SessionSourcePart{Role: "assistant", Kind: "thinking", Text: strings.Join(summary, "\n\n")})
	case "function_call":
		return emit(false, sdk.SessionSourcePart{Role: "assistant", Kind: "tool_use", Name: payload.Name, Text: sessionformat.Pretty(json.RawMessage(payload.Arguments))})
	case "custom_tool_call":
		return emit(false, sdk.SessionSourcePart{Role: "assistant", Kind: "tool_use", Name: payload.Name, Text: payload.Input})
	case "local_shell_call":
		return emit(false, sdk.SessionSourcePart{Role: "assistant", Kind: "tool_use", Name: "shell", Text: strings.Join(payload.Action.Command, " ")})
	case "function_call_output", "custom_tool_call_output":
		return emit(true, sdk.SessionSourcePart{Role: "tool", Kind: "tool_result", Text: sessionformat.CodexOutput(payload.Output)})
	}
	return true
}
