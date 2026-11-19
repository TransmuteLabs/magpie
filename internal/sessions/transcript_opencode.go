package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"unsafe"

	"github.com/tidwall/gjson"
)

// openCodeTranscript reads the conversation in message order from the same
// store the listing chose, including while a v1 to v2 migration is underway.
func openCodeTranscript(ctx context.Context, s Session, spokenOnly bool, add func(bool, Part) bool) error {
	storeMu.RLock()
	defer storeMu.RUnlock()
	// the store handles are shared with the reads that hold mu: they are
	// opened and given back under it, and read from without it
	mu.Lock()
	release := useDBs()
	files := openCodeFiles()
	mu.Unlock()
	defer func() {
		mu.Lock()
		release()
		mu.Unlock()
	}()
	for _, f := range files {
		if f.m().sid != s.ID {
			continue
		}
		if v, ok := f.m().oc.(ocV2DB); ok {
			rows, err := v.db.QueryContext(ctx, "SELECT type,data FROM session_message WHERE session_id=? AND type IN ('user','assistant') ORDER BY seq", s.ID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var role string
				var raw sql.RawBytes
				if err := rows.Scan(&role, &raw); err != nil {
					return err
				}
				var m struct {
					Text    string                    `json:"text"`
					Content openCodeTranscriptContent `json:"content"`
					Model   struct {
						ID string `json:"id"`
					} `json:"model"`
				}
				m.Content.spokenOnly = spokenOnly
				if err := json.Unmarshal(raw, &m); err != nil {
					return err
				}
				if role == "user" {
					if !add(true, textPart(role, m.Text)) {
						return nil
					}
					continue
				}
				for _, p := range m.Content.spoken {
					if !openCodeTranscriptText(role, m.Model.ID, p.Text, add) {
						return nil
					}
				}
				for _, p := range m.Content.all {
					if !openCodeTranscriptPart(role, m.Model.ID, p, add) {
						return nil
					}
				}
			}
			return rows.Err()
		}
		var messages []ocMessage
		for _, b := range f.m().oc.messages(f.m().sid) {
			if err := ctx.Err(); err != nil {
				return err
			}
			var m ocMessage
			if json.Unmarshal(b, &m) == nil {
				messages = append(messages, m)
			}
		}
		sort.SliceStable(messages, func(i, j int) bool {
			if messages[i].Time.Created != messages[j].Time.Created {
				return messages[i].Time.Created < messages[j].Time.Created
			}
			return messages[i].ID < messages[j].ID
		})
		for _, m := range messages {
			if err := ctx.Err(); err != nil {
				return err
			}
			if spokenOnly && m.Role != "user" && m.Role != "assistant" {
				continue
			}
			for _, p := range f.m().oc.parts(m.ID) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if spokenOnly {
					if text, ok := openCodeSpokenPart(p); ok &&
						!openCodeTranscriptText(m.Role, m.ModelID, text.Text, add) {
						return nil
					}
					continue
				}
				if !openCodeTranscriptPart(m.Role, m.ModelID, p, add) {
					return nil
				}
			}
		}
		return nil
	}
	return ErrNoTranscript
}

type openCodeTranscriptContent struct {
	spokenOnly bool
	all        []json.RawMessage
	spoken     []ocPart
}

func (c *openCodeTranscriptContent) UnmarshalJSON(raw []byte) error {
	c.spoken = c.spoken[:0]
	if !c.spokenOnly || len(raw) == 0 || raw[0] != '[' {
		return json.Unmarshal(raw, &c.all)
	}
	array := gjson.Parse(unsafe.String(unsafe.SliceData(raw), len(raw)))
	array.ForEach(func(_, item gjson.Result) bool {
		if item.IsObject() {
			if p, ok := openCodeSpokenPart(raw[item.Index : item.Index+len(item.Raw)]); ok {
				c.spoken = append(c.spoken, p)
			}
		}
		return true
	})
	return nil
}

func openCodeSpokenPart(raw []byte) (ocPart, bool) {
	var tag struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &tag) != nil || tag.Type != "text" {
		return ocPart{}, false
	}
	var part openCodePart
	ok := json.Unmarshal(raw, &part) == nil && !part.Synthetic && !part.Ignored
	return part.ocPart, ok
}

type openCodePart struct {
	ocPart
	Name  string `json:"name"`
	Tool  string `json:"tool"`
	State struct {
		Input   json.RawMessage `json:"input"`
		Output  string          `json:"output"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"state"`
}

func openCodeTranscriptText(role, model, text string, add func(bool, Part) bool) bool {
	part := textPart(role, text)
	if role == "assistant" {
		part.Model = model
	}
	return add(role == "user", part)
}

// openCodeTranscriptPart tells add one part of a message; model is the
// message's own (an assistant's modelID, or model.id in OpenCode 2), said
// with each of its assistant parts (#2253).
func openCodeTranscriptPart(role, model string, raw []byte, add func(bool, Part) bool) bool {
	var p openCodePart
	if json.Unmarshal(raw, &p) != nil || p.Synthetic || p.Ignored {
		return true
	}
	if p.Type == "text" {
		return openCodeTranscriptText(role, model, p.Text, add)
	}
	say := add
	add = func(input bool, q Part) bool {
		if q.Role == "assistant" {
			q.Model = model
		}
		return say(input, q)
	}
	switch p.Type {
	case "reasoning":
		return add(false, Part{Role: role, Kind: "thinking", Text: p.Text})
	case "tool":
		name := p.Name
		if name == "" {
			name = p.Tool
		}
		if !add(false, Part{Role: role, Kind: "tool_use", Name: name, Text: string(p.State.Input)}) {
			return false
		}
		if p.State.Output != "" && !add(false, Part{Role: "tool", Kind: "tool_result", Name: name, Text: p.State.Output}) {
			return false
		}
		for _, r := range p.State.Content {
			if r.Type == "text" && !add(false, Part{Role: "tool", Kind: "tool_result", Name: name, Text: r.Text}) {
				return false
			}
		}
	}
	return true
}
