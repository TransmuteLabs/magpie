package sessioncarry

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// ---- reading Claude Code

type ccCarryLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type ccCarryBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
	Source    struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// claudeCarryItems reads the branch a Claude Code session is on: each line
// names its parent, and the last thing said is where it stands. A
// compaction's boundary has no parent, so what came before it is left to
// the summary after it, as Claude Code does.
func claudeCarryItems(ctx context.Context, source io.Reader) ([]carryItem, error) {
	type node struct {
		parent string
		line   []byte // a user's or the assistant's, else nil
	}
	nodes := map[string]node{}
	leaf := ""
	err := sessionformat.EachLine(ctx, source, func(b []byte) {
		var l ccCarryLine
		if json.Unmarshal(b, &l) != nil || l.UUID == "" {
			return
		}
		n := node{parent: l.ParentUUID}
		if (l.Type == "user" || l.Type == "assistant") && !l.IsSidechain {
			n.line = append([]byte(nil), b...)
			leaf = l.UUID
		}
		nodes[l.UUID] = n
	})
	if err != nil {
		return nil, err
	}
	var chain [][]byte
	seen := map[string]bool{}
	for id := leaf; id != "" && !seen[id]; id = nodes[id].parent {
		seen[id] = true
		if n := nodes[id]; n.line != nil {
			chain = append(chain, n.line)
		}
	}
	var items []carryItem
	for i := len(chain) - 1; i >= 0; i-- {
		var l ccCarryLine
		if json.Unmarshal(chain[i], &l) != nil || l.IsMeta {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		items = append(items, claudeLineItems(l, at)...)
	}
	return items, nil
}

func claudeLineItems(l ccCarryLine, at time.Time) []carryItem {
	var text string
	if json.Unmarshal(l.Message.Content, &text) == nil {
		if l.Type == "user" && claudeOwnWords(text) {
			return nil
		}
		return []carryItem{{Kind: l.Type, Text: text, Time: at}}
	}
	var blocks []ccCarryBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil
	}
	var items []carryItem
	user := carryItem{Kind: "user", Time: at}
	var words []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if l.Type == "user" {
				if !claudeOwnWords(b.Text) {
					words = append(words, b.Text)
				}
			} else if strings.TrimSpace(b.Text) != "" {
				items = append(items, carryItem{Kind: "assistant", Text: b.Text, Time: at})
			}
		case "image":
			if l.Type == "user" && b.Source.Type == "base64" && b.Source.Data != "" {
				user.Images = append(user.Images, "data:"+b.Source.MediaType+";base64,"+b.Source.Data)
			}
		case "tool_use":
			it := carryItem{Kind: "call", Name: b.Name, ID: b.ID, Input: sessionformat.Pretty(b.Input), Time: at}
			if b.Name == "Bash" {
				var in struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(b.Input, &in) == nil {
					it.Shell = in.Command
				}
			}
			items = append(items, it)
		case "tool_result":
			items = append(items, carryItem{Kind: "result", ID: b.ToolUseID, Text: sessionformat.ResultText(b.Content), IsError: b.IsError, Time: at})
		}
		// thinking and redacted_thinking stay behind: see the top
	}
	if l.Type == "user" && (len(words) > 0 || len(user.Images) > 0) {
		user.Text = strings.Join(words, "\n\n")
		items = append([]carryItem{user}, items...)
	}
	return items
}

// claudeOwnWords is a user line Claude Code wrote itself: a slash
// command's name or its output, a reminder, the caveat over them.
func claudeOwnWords(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range []string{"<command-name>", "<command-message>", "<command-args>", "<local-command-", "<system-reminder>", "Caveat: The messages below were generated"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return t == ""
}

// claudeOutput renders items as a new Claude Code session of the folder
// cwd: <claude dir>/projects/<cwd as Claude names it>/<uuid>.jsonl, each
// line naming the one before it as its parent, its fields in Claude Code's
// order.
func claudeOutput(input sdk.SessionConversionInput, items []carryItem) (sdk.SessionConversionOutput, error) {
	cwd, now := input.Cwd, input.Now
	name, err := sessionformat.ProjectName(cwd)
	if err != nil {
		return sdk.SessionConversionOutput{}, err
	}
	id := uuid.NewString()
	version := input.TargetVersion
	if version == "" {
		version = "2.0.0"
	}
	write := func(ctx context.Context, output io.Writer) error {
		writer := lineWriter{ctx: ctx, output: output}
		parent := ""
		msgID := "" // the assistant message lines are part of, while it goes on
		line := func(typ string, message ord, at time.Time, extra ...any) string {
			u := uuid.NewString()
			l := o("parentUuid", nilIfEmpty(parent), "isSidechain", false, "type", typ, "message", message)
			l = append(l, o(extra...)...)
			l = append(l, o("uuid", u, "timestamp", carryStamp(at, now), "userType", "external",
				"entrypoint", "cli", "cwd", cwd, "sessionId", id, "version", version)...)
			writer.line(l)
			parent = u
			return u
		}
		assistant := func(block ord, stop string, at time.Time) string {
			if msgID == "" {
				msgID = "msg_" + randHex(12)
			}
			return line("assistant", o("id", msgID, "type", "message", "role", "assistant", "model", "<synthetic>",
				"content", []any{block}, "stop_reason", stop, "stop_sequence", nil,
				"usage", o("input_tokens", 0, "cache_creation_input_tokens", 0, "cache_read_input_tokens", 0, "output_tokens", 0)), at)
		}
		for _, it := range items {
			if writer.err != nil {
				break
			}
			switch it.Kind {
			case "user":
				msgID = ""
				var content any = it.Text
				if len(it.Images) > 0 {
					var blocks []any
					if it.Text != "" {
						blocks = append(blocks, o("type", "text", "text", it.Text))
					}
					for _, img := range it.Images {
						if mt, data, ok := splitDataURL(img); ok {
							blocks = append(blocks, o("type", "image", "source", o("type", "base64", "media_type", mt, "data", data)))
						}
					}
					content = blocks
				}
				line("user", o("role", "user", "content", content), it.Time)
			case "assistant":
				assistant(o("type", "text", "text", it.Text), "end_turn", it.Time)
			case "call":
				if it.Shell == "" {
					assistant(o("type", "text", "text", it.note()), "end_turn", it.Time)
					continue
				}
				command := it.Shell
				if it.Workdir != "" && it.Workdir != cwd {
					command = "cd " + shellQuote(it.Workdir) + " && " + command
				}
				callID := "toolu_" + randHex(12)
				by := assistant(o("type", "tool_use", "id", callID, "name", "Bash", "input", o("command", command)), "tool_use", it.Time)
				msgID = ""
				out := it.Result
				if !it.Done {
					out = "[no output was recorded]"
				}
				line("user", o("role", "user", "content", []any{o("tool_use_id", callID, "type", "tool_result", "content", out, "is_error", it.IsError)}), it.Time,
					"toolUseResult", o("stdout", out, "stderr", "", "interrupted", false, "isImage", false),
					"sourceToolAssistantUUID", by)
			}
		}
		return writer.err
	}
	return sdk.SessionConversionOutput{ID: id, Path: path.Join(name, id+".jsonl"), Write: write}, nil
}
