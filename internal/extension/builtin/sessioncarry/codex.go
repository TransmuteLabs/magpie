package sessioncarry

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/codexsummary"
	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// ---- reading Codex

type cxCarryPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Content   []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	} `json:"content"`
	Action struct {
		Command          []string `json:"command"`
		WorkingDirectory string   `json:"working_directory"`
	} `json:"action"`
	EncryptedContent   string            `json:"encrypted_content"`
	Message            string            `json:"message"`
	ReplacementHistory []json.RawMessage `json:"replacement_history"`
}

// codexCarryItems reads a Codex rollout's items in order. A compaction
// stands for what came before it: the history it kept, or the user's
// words before it and its summary, as Codex rebuilds it.
func codexCarryItems(ctx context.Context, source io.Reader) ([]carryItem, error) {
	var items []carryItem
	err := sessionformat.EachLine(ctx, source, func(b []byte) {
		var l struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(b, &l) != nil {
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		switch l.Type {
		case "response_item":
			var p cxCarryPayload
			if json.Unmarshal(l.Payload, &p) == nil {
				items = append(items, codexItem(p, at)...)
			}
		case "compacted":
			var p cxCarryPayload
			if json.Unmarshal(l.Payload, &p) != nil {
				return
			}
			if p.ReplacementHistory != nil {
				items = nil
				for _, raw := range p.ReplacementHistory {
					var q cxCarryPayload
					if json.Unmarshal(raw, &q) == nil {
						items = append(items, codexItem(q, at)...)
					}
				}
			} else if p.Message != "" {
				var kept []carryItem
				for _, it := range items {
					if it.Kind == "user" {
						kept = append(kept, it)
					}
				}
				items = append(kept, carryItem{Kind: "user", Text: codexsummary.Prefix + "\n" + p.Message, Time: at})
			}
		}
	})
	return items, err
}

func codexItem(p cxCarryPayload, at time.Time) []carryItem {
	switch p.Type {
	case "message":
		var words, images []string
		for _, c := range p.Content {
			switch c.Type {
			case "input_text", "output_text", "text":
				words = append(words, c.Text)
			case "input_image":
				if strings.HasPrefix(c.ImageURL, "data:") {
					images = append(images, c.ImageURL)
				}
			}
		}
		text := strings.Join(words, "\n")
		switch p.Role {
		case "user":
			if codexOwnWords(text) && len(images) == 0 {
				return nil
			}
			if codexOwnWords(text) {
				text = ""
			}
			return []carryItem{{Kind: "user", Text: text, Images: images, Time: at}}
		case "assistant":
			if strings.TrimSpace(text) == "" {
				return nil
			}
			return []carryItem{{Kind: "assistant", Text: text, Time: at}}
		}
	case "function_call":
		it := carryItem{Kind: "call", Name: p.Name, ID: p.CallID, Input: sessionformat.Pretty(json.RawMessage(p.Arguments)), Time: at}
		var a struct {
			Cmd     string          `json:"cmd"`
			Command json.RawMessage `json:"command"`
			Workdir string          `json:"workdir"`
		}
		if json.Unmarshal([]byte(p.Arguments), &a) == nil {
			switch p.Name {
			case "exec_command":
				it.Shell = a.Cmd
			case "shell", "container.exec":
				var argv []string
				if json.Unmarshal(a.Command, &argv) == nil {
					it.Shell = shellLine(argv)
				}
			case "shell_command":
				json.Unmarshal(a.Command, &it.Shell)
			}
			it.Workdir = a.Workdir
		}
		return []carryItem{it}
	case "custom_tool_call":
		return []carryItem{{Kind: "call", Name: p.Name, ID: p.CallID, Input: p.Input, Time: at}}
	case "local_shell_call":
		return []carryItem{{Kind: "call", Name: "shell", ID: p.CallID, Input: strings.Join(p.Action.Command, " "),
			Shell: shellLine(p.Action.Command), Workdir: p.Action.WorkingDirectory, Time: at}}
	case "function_call_output", "custom_tool_call_output":
		return []carryItem{{Kind: "result", ID: p.CallID, Text: sessionformat.CodexOutput(p.Output), Time: at}}
	case "compaction", "compaction_summary":
		// magpie's own summary is read; OpenAI's is encrypted for OpenAI only
		if msg, ours := codexsummary.Message(p.EncryptedContent); ours && msg != nil {
			c, _ := msg["content"].([]map[string]any)
			if len(c) > 0 {
				if t, _ := c[0]["text"].(string); t != "" {
					return []carryItem{{Kind: "user", Text: t, Time: at}}
				}
			}
		}
	}
	// reasoning stays behind: see the top
	return nil
}

// codexOwnWords is a user message Codex wrote itself: its context (the
// folder, AGENTS.md, a turn's interruption), not the user's words.
func codexOwnWords(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "# AGENTS.md")
}

// shellLine is a command's argv as the line a shell would run: the script
// of `bash -lc <script>`, else the words quoted.
func shellLine(argv []string) string {
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2]
	}
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = a
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
			q[i] = shellQuote(a)
		}
	}
	return strings.Join(q, " ")
}

// codexOutputDocument renders items as a new Codex session of the folder cwd:
// <codex home>/sessions/YYYY/MM/DD/rollout-<local time>-<uuid v7>.jsonl.
// Its session_meta says what a rollout's must (the provider is the one
// Codex uses now, so its history lists it) and no base_instructions: Codex
// gives its own. Each user message and answer is written as Codex's own
// event too, which its TUI shows a resumed session's history from. Codex
// adds the thread to its state database itself when it opens it.
func codexOutputDocument(input sdk.SessionConversionInput, items []carryItem) (sdk.SessionConversionOutput, error) {
	cwd, now := input.Cwd, input.Now
	u, err := uuid.NewV7()
	if err != nil {
		return sdk.SessionConversionOutput{}, err
	}
	id := u.String()
	local := now.Local()
	name := path.Join(local.Format("2006"), local.Format("01"), local.Format("02"),
		"rollout-"+local.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	version := input.TargetVersion
	if version == "" {
		version = "0.0.0"
	}
	write := func(ctx context.Context, output io.Writer) error {
		writer := lineWriter{ctx: ctx, output: output}
		stamp := carryStamp(now, now)
		writer.line(o("timestamp", stamp, "type", "session_meta", "payload", o(
			"session_id", id, "id", id, "timestamp", stamp, "cwd", cwd, "originator", "codex_cli_rs",
			"cli_version", version, "source", "cli", "model_provider", input.TargetProvider)))
		add := func(at time.Time, typ string, payload ord) {
			writer.line(o("timestamp", carryStamp(at, now), "type", typ, "payload", payload))
		}
		say := func(at time.Time, text string) {
			add(at, "response_item", o("type", "message", "role", "assistant",
				"content", []any{o("type", "output_text", "text", text)}))
			add(at, "event_msg", o("type", "agent_message", "message", text))
		}
		for _, it := range items {
			if writer.err != nil {
				break
			}
			switch it.Kind {
			case "user":
				content := []any{}
				if it.Text != "" {
					content = append(content, o("type", "input_text", "text", it.Text))
				}
				images := []string{}
				for _, img := range it.Images {
					if _, _, ok := splitDataURL(img); ok {
						content = append(content, o("type", "input_image", "image_url", img))
						images = append(images, img)
					}
				}
				add(it.Time, "response_item", o("type", "message", "role", "user", "content", content))
				add(it.Time, "event_msg", o("type", "user_message", "message", it.Text,
					"images", images, "local_images", []any{}, "text_elements", []any{}))
			case "assistant":
				say(it.Time, it.Text)
			case "call":
				if it.Shell == "" {
					say(it.Time, it.note())
					continue
				}
				callID := "call_" + randHex(12)
				workdir := it.Workdir
				if workdir == "" {
					workdir = cwd
				}
				add(it.Time, "response_item", o("type", "function_call", "name", "exec_command",
					"arguments", string(jsonLine(o("cmd", it.Shell, "workdir", workdir))), "call_id", callID))
				out := it.Result
				if !it.Done {
					out = "[no output was recorded]"
				}
				add(it.Time, "response_item", o("type", "function_call_output", "call_id", callID, "output", out))
			}
		}
		return writer.err
	}
	return sdk.SessionConversionOutput{ID: id, Path: name, Write: write}, nil
}
