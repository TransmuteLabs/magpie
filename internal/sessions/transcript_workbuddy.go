package sessions

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"strings"

	"github.com/yetone/magpie/internal/lazyre"
)

// wbTranscript tells add what a WorkBuddy, WorkBuddy AI or CodeBuddy Code
// session said, from the same lines its summary is read from
// (workbuddyLine): message lines (a user's input_text, a reply's
// output_text), a function_call with its arguments, and its
// function_call_result.
func wbTranscript(ctx context.Context, path string, spokenOnly bool, add func(bool, Part) bool) error {
	names := map[string]string{} // a call's tool name, by its callId
	_, err := scanThinContext(ctx, path, 0, nil, nil, func(b []byte, _, _ int64) bool {
		if !bytes.Contains(b, []byte(`"type":"message"`)) &&
			(spokenOnly || !bytes.Contains(b, []byte(`"type":"function_call`))) {
			return true
		}
		var l struct {
			wbLine
			Name      string          `json:"name"`
			CallID    string          `json:"callId"`
			Arguments json.RawMessage `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(b, &l) != nil || spokenOnly && l.Type != "message" {
			return true
		}
		for _, p := range wbParts(l.Type, l.Role, l.Content, l.Name, l.CallID, l.Arguments, l.Output, l.ProviderData.Model, names) {
			if spokenOnly && !said(p) {
				continue
			}
			if !add(p.Role != "assistant", p) {
				return false
			}
		}
		return true
	})
	return err
}

func wbParts(typ, role string, content json.RawMessage, name, callID string, args, output json.RawMessage, model string, names map[string]string) []Part {
	switch typ {
	case "message":
		if role != "user" && role != "assistant" {
			return nil
		}
		// one part to a block: a user message holds WorkBuddy's own
		// <system-reminder> and @scene: blocks beside what was typed
		var parts []Part
		for _, text := range wbTexts(content) {
			if role == "user" && wbScene.MatchString(strings.TrimSpace(text)) {
				continue
			}
			if role == "user" {
				if split, ok := wbSplit(text); ok {
					parts = append(parts, split...)
					continue
				}
			}
			p := textPart(role, text)
			if role == "assistant" {
				p.Model = model
			}
			parts = append(parts, p)
		}
		return parts
	case "function_call":
		names[callID] = name
		var s string
		if json.Unmarshal(args, &s) == nil {
			args = json.RawMessage(s)
		}
		text := string(args)
		if json.Valid(args) {
			text = pretty(args)
		}
		return []Part{{Role: "assistant", Kind: "tool_use", Name: name, Text: text, Model: model}}
	case "function_call_result":
		var o struct{ Text string }
		text := resultText(output)
		if json.Unmarshal(output, &o) == nil && o.Text != "" {
			text = o.Text
		}
		return []Part{{Role: "tool", Kind: "tool_result", Name: cmp.Or(names[callID], name), Text: text}}
	}
	return nil
}

// wbTexts are the words of a message: its string, or each of its text
// blocks.
func wbTexts(content json.RawMessage) []string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if (b.Type == "input_text" || b.Type == "output_text" || b.Type == "text") && strings.TrimSpace(b.Text) != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

var wbReminderSection = lazyre.New(`^<([[:alpha:]_][[:alnum:]_-]*)(?:[ \t]+.*)?>$`)

// wbTyped recognizes WorkBuddy's user-context reminder and the query that
// follows its outer close. The reminder's sections can quote that boundary;
// an incomplete reminder or a second possible boundary remains context.
func wbTyped(text string) (typed, rest string, ok bool) {
	const reminderOpen = `<system-reminder data-role="user-context">`
	const reminderClose = "</system-reminder>"
	const queryOpen = "<user_query>"
	text = strings.TrimSpace(text)
	body, startsReminder := strings.CutPrefix(text, reminderOpen)
	if !startsReminder {
		return "", text, false
	}
	openingTail, remaining, _ := strings.Cut(body, "\n")
	if strings.TrimSpace(openingTail) != "" {
		return "", text, false
	}
	var sectionName, sectionClose string
	sectionDepth := 0
	for remaining != "" {
		raw, after, _ := strings.Cut(remaining, "\n")
		line := strings.TrimSpace(raw)
		switch {
		case sectionDepth > 0:
			if line == sectionClose {
				sectionDepth--
			} else if strings.HasPrefix(line, "<"+sectionName) && !strings.HasSuffix(line, "/>") {
				if match := wbReminderSection.FindStringSubmatch(line); match != nil && match[1] == sectionName {
					sectionDepth++
				}
			}
		case line == reminderClose && strings.HasPrefix(raw, reminderClose):
			query, opensQuery := strings.CutPrefix(strings.TrimSpace(after), queryOpen)
			typed, closesQuery := strings.CutSuffix(query, "</user_query>")
			if !opensQuery || !closesQuery {
				return "", text, true
			}
			for suffix := query; ; {
				_, tail, found := strings.Cut(suffix, "\n"+reminderClose)
				if !found {
					break
				}
				closingTail, next, hasLine := strings.Cut(tail, "\n")
				if hasLine && strings.TrimSpace(closingTail) == "" && strings.HasPrefix(strings.TrimSpace(next), queryOpen) {
					return "", text, true
				}
				suffix = tail
			}
			return strings.TrimSpace(typed), text[:len(text)-len(remaining)+len(reminderClose)], true
		case strings.HasPrefix(raw, "</") && strings.HasSuffix(line, ">"):
			return "", text, true
		case strings.HasPrefix(line, "<") && !strings.HasSuffix(line, "/>"):
			if match := wbReminderSection.FindStringSubmatch(line); match != nil {
				sectionName, sectionClose, sectionDepth = match[1], "</"+match[1]+">", 1
			}
		}
		remaining = after
	}
	return "", text, true
}

// wbSplit returns the reminder context and any user text in a recognized block.
func wbSplit(text string) ([]Part, bool) {
	typed, rest, ok := wbTyped(text)
	if !ok {
		return nil, false
	}
	var parts []Part
	if rest != "" {
		parts = append(parts, Part{Role: "user", Kind: "context", Text: rest})
	}
	if typed != "" {
		parts = append(parts, Part{Role: "user", Kind: "text", Text: typed})
	}
	return parts, true
}
