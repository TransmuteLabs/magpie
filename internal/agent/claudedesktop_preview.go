package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/desktopdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func desktopDisconnectPreview(home string) ([]FileChange, error) {
	paths := desktopSets(desktopdir.OS, home, os.Getenv)
	stashMu.Lock()
	defer stashMu.Unlock()
	if !desktopWiredAny(paths) {
		return nil, nil
	}
	stashFile, prior := stashPath(), stashLoad()
	inputs, err := desktopSettingsInputs(paths, stashFile, prior)
	if err != nil {
		return nil, err
	}
	change := desktopChangeAll(paths, desktopDisconnect)
	before, plan, err := extension.PreviewLinkedAgentPlan(func(snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, _, err := desktopSettingsPlan(paths, stashFile, prior, snapshot, change)
		return plan, err
	}, inputs...)
	if err != nil {
		return nil, err
	}
	files := make(map[string]sdk.ActionSettingFile, len(before.Files))
	for _, file := range before.Files {
		files[file.Path] = file
	}
	var changes []FileChange
	for _, change := range plan.Files {
		if change.Path == stashFile {
			continue
		}
		path := change.Path
		if rel, err := filepath.Rel(home, path); err == nil && filepath.IsLocal(rel) {
			path = "~/" + filepath.ToSlash(rel)
		}
		if change.Remove && edit.RemoveUnlinks(change.Path) {
			changes = append(changes, FileChange{Path: path, Removed: true})
			continue
		}
		file := files[change.Path]
		lines, err := desktopPreviewLines(file.Body, change.Body)
		if err != nil {
			return nil, edit.AtFile(change.Path, err)
		}
		if change.Remove && len(lines) == 0 && len(file.Body) > 0 {
			var text string
			if len(bytes.TrimSpace(edit.TrimBOM(file.Body))) > 0 {
				text = "{}"
			}
			lines = append(lines, LineDiff{Op: "-", Text: text})
		}
		if len(lines) > 0 {
			changes = append(changes, FileChange{Path: path, Created: !file.Exists, Lines: lines})
		}
	}
	return changes, nil
}

func desktopPreviewLines(before, after []byte) ([]LineDiff, error) {
	was, err := desktopFields(before)
	if err != nil {
		return nil, err
	}
	now, err := desktopFields(after)
	if err != nil {
		return nil, err
	}
	fields := maps.Clone(was)
	maps.Copy(fields, now)
	var lines []LineDiff
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		old, next := was[name], now[name]
		switch {
		case bytes.Equal(old, next), desktopSameJSON(old, next):
		case next == nil:
			lines = append(lines, LineDiff{Op: "-", Text: desktopPreviewField(name, old)})
		case old == nil:
			lines = append(lines, LineDiff{Op: "+", Text: desktopPreviewField(name, next)})
		default:
			lines = append(lines, LineDiff{Op: "~", Text: desktopPreviewField(name, next), Was: desktopPreviewField(name, old)})
		}
	}
	return lines, nil
}

func desktopPreviewField(name string, raw json.RawMessage) string {
	var value any = "••••"
	switch name {
	case "deploymentMode":
		switch mode := desktopString(raw); mode {
		case "1p", "3p", "any":
			value = mode
		}
	case "locale":
		if locale := desktopString(raw); desktopLocaleName.MatchString(locale) {
			value = locale
		}
	case "appliedId":
		if id := desktopString(raw); id != "" && !strings.ContainsAny(id, `/\:`) && !strings.HasPrefix(id, ".") {
			value = id
		}
	case "entries":
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) == nil && entries != nil {
			ids := make([]string, len(entries))
			for index, entry := range entries {
				ids[index] = desktopEntryID(entry)
			}
			value = ids
		}
	case "mcpServers":
		var servers map[string]json.RawMessage
		if json.Unmarshal(raw, &servers) == nil && servers != nil {
			value = slices.Sorted(maps.Keys(servers))
		}
	}
	shown, _ := json.Marshal(value)
	return fmt.Sprintf("%q: %s", name, shown)
}
