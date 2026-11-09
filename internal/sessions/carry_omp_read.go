package sessions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

// An entry is retained even when its payload is not used: metadata entries
// can be parents, and off-branch entries can record the active leaf.
type ompCarryEntry struct {
	Type                         string                     `json:"type"`
	ID                           string                     `json:"id"`
	ParentID                     *string                    `json:"parentId"`
	ActiveLeafID                 json.RawMessage            `json:"activeLeafId"`
	Timestamp                    string                     `json:"timestamp"`
	Message                      json.RawMessage            `json:"message"`
	Summary                      string                     `json:"summary"`
	FirstKeptEntryID             string                     `json:"firstKeptEntryId"`
	ProviderReplayThroughEntryID string                     `json:"providerReplayThroughEntryId"`
	Method                       string                     `json:"method"`
	PreserveData                 map[string]json.RawMessage `json:"preserveData"`
	Details                      json.RawMessage            `json:"details"`
	Unreadable                   bool                       `json:"-"`
}

type ompCarryMessage struct {
	Role               string          `json:"role"`
	Content            json.RawMessage `json:"content"`
	Timestamp          *int64          `json:"timestamp"`
	Attribution        string          `json:"attribution"`
	Synthetic          bool            `json:"synthetic"`
	ToolCallID         string          `json:"toolCallId"`
	IsError            bool            `json:"isError"`
	Command            string          `json:"command"`
	Code               string          `json:"code"`
	Output             string          `json:"output"`
	ExitCode           *int            `json:"exitCode"`
	Cancelled          bool            `json:"cancelled"`
	ExcludeFromContext bool            `json:"excludeFromContext"`
	Files              []struct {
		Image *ompCarryBlock `json:"image"`
	} `json:"files"`
}

type ompCarryBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Narration bool            `json:"narration"`
	Data      string          `json:"data"`
	MimeType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
}

// ompCarryItems follows replay's active parent chain, then rebuilds its
// portable context. Images are not opened until after both selections.
func ompCarryItems(path string) ([]carryItem, error) {
	var entries []ompCarryEntry
	byID := make(map[string]int)
	if err := eachLine(path, func(line []byte) {
		var envelope struct {
			Type         string          `json:"type"`
			ID           string          `json:"id"`
			ParentID     *string         `json:"parentId"`
			ActiveLeafID json.RawMessage `json:"activeLeafId"`
			Timestamp    string          `json:"timestamp"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.ID == "" || envelope.Type == "session" || envelope.Type == "title" {
			return
		}
		entry := ompCarryEntry{Type: envelope.Type, ID: envelope.ID, ParentID: envelope.ParentID,
			ActiveLeafID: envelope.ActiveLeafID, Timestamp: envelope.Timestamp}
		if entry.Type == "message" || entry.Type == "compaction" || entry.Type == "branch_summary" {
			var payload ompCarryEntry
			if json.Unmarshal(line, &payload) == nil {
				entry = payload
			} else if entry.Type == "compaction" {
				// An unreadable boundary cannot safely stand in for history.
				entry.Unreadable = true
			}
		}
		byID[entry.ID] = len(entries)
		entries = append(entries, entry)
	}); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	// Absence means self, while explicit null clears the selected branch.
	leafOf := func(entry ompCarryEntry) (string, bool) {
		if len(entry.ActiveLeafID) == 0 {
			return entry.ID, false
		}
		if string(entry.ActiveLeafID) == "null" {
			return "", true
		}
		var leaf string
		if json.Unmarshal(entry.ActiveLeafID, &leaf) != nil {
			return "\x00invalid-active-leaf", false
		}
		return leaf, false
	}
	last := entries[len(entries)-1]
	leaf, cleared := leafOf(last)
	if !cleared {
		if _, ok := byID[leaf]; !ok {
			leaf = ""
			for i := len(entries) - 1; i >= 0; i-- {
				selected, null := leafOf(entries[i])
				if !null && selected == entries[i].ID {
					leaf = entries[i].ID
					break
				}
			}
		}
	}
	var reverse []ompCarryEntry
	seen := make(map[string]bool)
	for id := leaf; id != ""; {
		if seen[id] {
			return nil, fmt.Errorf("omp session parent cycle at %q", id)
		}
		index, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("omp session missing parent %q", id)
		}
		seen[id] = true
		entry := entries[index]
		reverse = append(reverse, entry)
		if entry.ParentID == nil {
			break
		}
		if *entry.ParentID == "" {
			return nil, fmt.Errorf("omp session missing parent %q", *entry.ParentID)
		}
		id = *entry.ParentID
	}
	branch := reverse
	for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
		branch[i], branch[j] = branch[j], branch[i]
	}

	root, compact := 0, -1
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == "reset_boundary" {
			root = i + 1
			break
		}
	}
	for i := len(branch) - 1; i >= root; i-- {
		if branch[i].Type == "compaction" && ompCarryPortable(branch[i]) {
			compact = i
			break
		}
	}
	retained := root
	if compact >= 0 {
		c := branch[compact]
		retained = compact
		for i := 0; i < compact; i++ {
			if c.FirstKeptEntryID != "" && branch[i].ID == c.FirstKeptEntryID {
				retained = i
				break
			}
		}
		if retained == compact && c.FirstKeptEntryID == "" && ompCarryPresent(c.PreserveData["anthropicCompaction"]) && c.ProviderReplayThroughEntryID != "" {
			for i := 0; i < compact; i++ {
				if branch[i].ID == c.ProviderReplayThroughEntryID {
					retained = i + 1
					break
				}
			}
		}
		if retained < root {
			retained = root
		}
	}

	resolver := ompCarryBlobs{source: path, memo: make(map[string]string)}
	var items []carryItem
	// Emit the selected summary before the retained tail without copying
	// either region of the branch. Empty summaries still enforce the cutoff.
	if compact >= 0 && branch[compact].Summary != "" {
		at, _ := time.Parse(time.RFC3339Nano, branch[compact].Timestamp)
		items = append(items, ompCarryUserText(branch[compact].Summary, at))
	}
	var executions []int
	for _, entry := range branch[retained:] {
		at, stampErr := time.Parse(time.RFC3339Nano, entry.Timestamp)
		switch entry.Type {
		case "branch_summary":
			if entry.Summary != "" && !strings.Contains(entry.Summary, "¶think:") {
				items = append(items, ompCarryUserText(entry.Summary, at))
			}
		case "message":
			var m ompCarryMessage
			if json.Unmarshal(entry.Message, &m) != nil {
				continue
			}
			if stampErr != nil && m.Timestamp != nil {
				at = time.UnixMilli(*m.Timestamp)
			}
			added, err := ompCarryMessageItems(entry.ID, m, at, &resolver)
			if err != nil {
				return nil, fmt.Errorf("omp entry %q: %w", entry.ID, err)
			}
			if len(added) == 2 && (m.Role == "bashExecution" || m.Role == "pythonExecution") {
				executions = append(executions, len(items))
			}
			items = append(items, added...)
		}
	}

	// Native execution messages lack tool IDs. Their local pair IDs must not
	// collide with any tool ID recorded by the provider.
	if len(executions) != 0 {
		generated := make(map[int]bool, len(executions)*2)
		for _, i := range executions {
			generated[i], generated[i+1] = true, true
		}
		used := make(map[string]bool)
		for i, item := range items {
			if !generated[i] && item.ID != "" {
				used[item.ID] = true
			}
		}
		for _, i := range executions {
			id := items[i].ID
			for used[id] {
				id += ":"
			}
			items[i].ID, items[i+1].ID = id, id
			used[id] = true
		}
	}

	// pairCarry intentionally preserves missing-output behavior for the other
	// readers. For omp, only calls with a later selected result may cross.
	results := make(map[string]bool)
	keep := make([]bool, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		switch it.Kind {
		case "result":
			if it.ID != "" {
				results[it.ID] = true
			}
			keep[i] = true
		case "call":
			keep[i] = it.ID != "" && results[it.ID]
			if keep[i] {
				delete(results, it.ID)
			}
		default:
			keep[i] = true
		}
	}
	out := items[:0]
	for i, it := range items {
		if keep[i] {
			out = append(out, it)
		}
	}
	return out, nil
}

func ompCarryPresent(raw json.RawMessage) bool {
	return len(raw) != 0 && string(raw) != "null"
}

func ompCarryPortable(entry ompCarryEntry) bool {
	var details struct {
		Kind string `json:"kind"`
	}
	json.Unmarshal(entry.Details, &details)
	return !entry.Unreadable && entry.Method != "snapcompact" && details.Kind != "experimental-context-rollover" &&
		!strings.Contains(entry.Summary, "¶think:") &&
		!ompCarryPresent(entry.PreserveData["openaiRemoteCompaction"]) &&
		!ompCarryPresent(entry.PreserveData["snapcompact"])
}

func ompCarryUserText(text string, at time.Time) carryItem {
	return carryItem{Kind: "user", Content: []carryContent{{Type: "text", Text: text}}, Time: at}
}

func ompCarryMessageItems(id string, m ompCarryMessage, at time.Time, blobs *ompCarryBlobs) ([]carryItem, error) {
	switch m.Role {
	case "user":
		if m.Synthetic || m.Attribution == "agent" {
			return nil, nil
		}
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			if text == "" {
				return nil, nil
			}
			return []carryItem{ompCarryUserText(text, at)}, nil
		}
		var blocks []ompCarryBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil, nil
		}
		user := carryItem{Kind: "user", Time: at}
		for _, block := range blocks {
			switch block.Type {
			case "text":
				user.Content = append(user.Content, carryContent{Type: "text", Text: block.Text})
			case "image":
				image, err := blobs.image(block)
				if err != nil {
					return nil, err
				}
				user.Content = append(user.Content, image)
			}
		}
		if len(user.Content) != 0 {
			return []carryItem{user}, nil
		}
	case "fileMention":
		user := carryItem{Kind: "user", Time: at}
		for _, file := range m.Files {
			if file.Image != nil {
				image, err := blobs.image(*file.Image)
				if err != nil {
					return nil, err
				}
				user.Content = append(user.Content, image)
			}
		}
		if len(user.Content) != 0 {
			return []carryItem{user}, nil
		}
	case "assistant":
		var blocks []ompCarryBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil, nil
		}
		var out []carryItem
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if block.Text != "" {
					out = append(out, carryItem{Kind: "assistant", Text: block.Text, Time: at})
				}
			case "thinking":
				if block.Narration && block.Thinking != "" {
					out = append(out, carryItem{Kind: "assistant", Text: block.Thinking, Time: at})
				}
			case "toolCall":
				args := block.Arguments
				if !ompCarryPresent(args) {
					args = block.Input
				}
				call := carryItem{Kind: "call", ID: block.ID, Name: block.Name, Input: pretty(args), Time: at}
				if block.Name == "bash" {
					var input struct {
						Command string `json:"command"`
						Cwd     string `json:"cwd"`
					}
					if json.Unmarshal(args, &input) == nil {
						call.Shell, call.Workdir = input.Command, input.Cwd
					}
				}
				out = append(out, call)
			}
		}
		return out, nil
	case "toolResult":
		return []carryItem{{Kind: "result", ID: m.ToolCallID, Text: resultText(m.Content), IsError: m.IsError, Time: at}}, nil
	case "bashExecution", "pythonExecution":
		if m.ExcludeFromContext {
			return nil, nil
		}
		call := carryItem{Kind: "call", ID: "\x00omp-execution:" + id, Time: at}
		if m.Role == "bashExecution" {
			call.Name, call.Shell, call.Input = "bash", m.Command, m.Command
		} else {
			call.Name, call.Input = "python", m.Code
		}
		result := carryItem{Kind: "result", ID: call.ID, Text: m.Output, IsError: m.Cancelled || (m.ExitCode != nil && *m.ExitCode != 0), Time: at}
		return []carryItem{call, result}, nil
	}
	return nil, nil
}

// ompCarryBlobs searches only the source's managed store and known omp
// stores, without making them. Memoization lasts for one conversion only.
type ompCarryBlobs struct {
	source string
	roots  []string
	memo   map[string]string
}

func (b *ompCarryBlobs) image(block ompCarryBlock) (carryContent, error) {
	if block.MimeType == "" || block.Data == "" {
		return carryContent{}, fmt.Errorf("omp image is missing data or media type")
	}
	data := block.Data
	if len(data) >= len("blob:") && strings.EqualFold(data[:len("blob:")], "blob:") {
		const prefix = "blob:sha256:"
		hash, ok := strings.CutPrefix(data, prefix)
		if !ok || len(hash) != 64 {
			return carryContent{}, fmt.Errorf("invalid omp image blob reference %q", data)
		}
		for _, c := range hash {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return carryContent{}, fmt.Errorf("invalid omp image blob reference %q", data)
			}
		}
		if value, ok := b.memo[hash]; ok {
			data = value
		} else {
			if b.roots == nil {
				b.roots = ompCarryBlobRoots(b.source)
			}
			var lastErr error
			found := false
			for _, root := range b.roots {
				raw, err := os.ReadFile(filepath.Join(root, hash))
				if err != nil {
					lastErr = err
					continue
				}
				data = base64.StdEncoding.EncodeToString(raw)
				b.memo[hash] = data
				found = true
				break
			}
			if !found {
				if lastErr != nil {
					return carryContent{}, fmt.Errorf("unresolved omp image blob %q: %w", hash, lastErr)
				}
				return carryContent{}, fmt.Errorf("unresolved omp image blob %q: no managed blob store", hash)
			}
		}
	}
	return carryContent{Type: "image", Data: data, MimeType: block.MimeType}, nil
}

func ompCarryBlobRoots(source string) []string {
	var roots []string
	seen := make(map[string]bool)
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if !seen[dir] {
			seen[dir] = true
			roots = append(roots, dir)
		}
	}
	addProfiles := func(profiles string, suffix ...string) {
		entries, err := os.ReadDir(profiles)
		if err != nil {
			return
		}
		for _, entry := range entries {
			parts := append([]string{profiles, entry.Name()}, suffix...)
			dir := filepath.Join(parts...)
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				add(dir)
			}
		}
	}
	if source, err := filepath.Abs(source); err == nil {
		for dir := filepath.Dir(source); ; dir = filepath.Dir(dir) {
			if filepath.Base(dir) == "sessions" {
				add(filepath.Join(filepath.Dir(dir), "blobs"))
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	if home, err := appdir.Home(); err == nil {
		add(filepath.Join(OmpDir(), "blobs"))
		addProfiles(filepath.Join(home, ".omp", "profiles"), "agent", "blobs")
	}
	if runtime.GOOS != "windows" {
		if xdg := appdir.Getenv("XDG_DATA_HOME"); xdg != "" && filepath.IsAbs(xdg) {
			base := filepath.Join(xdg, "omp")
			add(filepath.Join(base, "blobs"))
			addProfiles(filepath.Join(base, "profiles"), "blobs")
		}
	}
	return roots
}
