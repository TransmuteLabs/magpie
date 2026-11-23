package pisession

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TreeCodec() sdk.SessionTreeCodec {
	return sdk.SessionTreeCodec{Inspect: inspect, Decode: decode}
}

type treeLine[Message any] struct {
	Type     string   `json:"type"`
	ID       string   `json:"id"`
	ParentID string   `json:"parentId"`
	Message  *Message `json:"message"`
}

type messageHead struct {
	Role     string `json:"role"`
	ToolName string `json:"toolName"`
	Model    string `json:"model"`
}

type transcriptMessage struct {
	messageHead
	Content sessionformat.TranscriptText `json:"content"`
}

type entry struct {
	treeLine[transcriptMessage]
	Summary string `json:"summary"`
}

type summaryHead struct{}

func (*summaryHead) UnmarshalJSON(raw []byte) error {
	if raw[0] == '"' || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return errors.New("Pi summary must be a string")
}

func inspect(line []byte, spokenOnly bool) (sdk.SessionTreeNode, bool) {
	if len(line) != 0 && line[0] == '{' {
		if head, ok := projectTreeHead(line); ok {
			if len(head.id) == 0 || bytes.Equal(head.kind, []byte("session")) || bytes.Equal(head.kind, []byte("title")) {
				return sdk.SessionTreeNode{}, false
			}
			hasContent := treeHasContent(string(head.kind), string(head.role), spokenOnly)
			return sdk.SessionTreeNode{ID: string(head.id), ParentID: string(head.parent), HasContent: hasContent}, true
		}
	}
	var head struct {
		treeLine[messageHead]
		Summary summaryHead `json:"summary"`
	}
	if json.Unmarshal(line, &head) != nil || head.ID == "" || head.Type == "session" || head.Type == "title" {
		return sdk.SessionTreeNode{}, false
	}
	var role string
	if head.Message != nil {
		role = head.Message.Role
	}
	return sdk.SessionTreeNode{ID: head.ID, ParentID: head.ParentID, HasContent: treeHasContent(head.Type, role, spokenOnly)}, true
}

func treeHasContent(kind, role string, spokenOnly bool) bool {
	if spokenOnly {
		return kind == "message" && (role == "user" || role == "assistant")
	}
	return kind == "message" || kind == "compaction" || kind == "branch_summary"
}

func decode(line []byte, spokenOnly bool) (sdk.SessionTreeRecord, error) {
	var record entry
	if spokenOnly {
		record.Message = &transcriptMessage{Content: sessionformat.TranscriptText{SpokenOnly: true}}
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return sdk.SessionTreeRecord{}, err
	}
	return sdk.SessionTreeRecord{ID: record.ID, ParentID: record.ParentID, Parts: record.parts}, nil
}

func (record entry) parts() []sdk.SessionSourcePart {
	switch record.Type {
	case "compaction", "branch_summary":
		return []sdk.SessionSourcePart{{Role: "user", Kind: "context", Text: record.Summary}}
	case "message":
	default:
		return nil
	}
	message := record.Message
	if message == nil {
		return nil
	}
	role := message.Role
	switch role {
	case "user", "assistant":
	case "toolResult":
		return []sdk.SessionSourcePart{{Role: "tool", Kind: "tool_result", Name: message.ToolName, Text: sessionformat.ResultText(message.Content.Raw)}}
	default:
		return nil
	}
	parts := message.parts(role)
	if role == "assistant" {
		for index := range parts {
			parts[index].Model = message.Model
		}
	}
	return parts
}

func (message *transcriptMessage) parts(role string) []sdk.SessionSourcePart {
	if message.Content.SpokenOnly {
		return message.Content.SpokenParts(role)
	}
	var text string
	if json.Unmarshal(message.Content.Raw, &text) == nil {
		return []sdk.SessionSourcePart{sessionformat.TextPart(role, text)}
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(message.Content.Raw, &blocks) != nil {
		return nil
	}
	var parts []sdk.SessionSourcePart
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, sessionformat.TextPart(role, block.Text))
		case "thinking":
			parts = append(parts, sdk.SessionSourcePart{Role: role, Kind: "thinking", Text: block.Thinking})
		case "toolCall":
			parts = append(parts, sdk.SessionSourcePart{Role: role, Kind: "tool_use", Name: block.Name, Text: sessionformat.Pretty(block.Arguments)})
		case "image":
			parts = append(parts, sdk.SessionSourcePart{Role: role, Kind: "image", Text: "[image]"})
		}
	}
	return parts
}
