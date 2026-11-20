package memorydigest

import (
	"slices"
	"strings"
	"unicode/utf8"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Renderer() sdk.MemoryDigestRenderer {
	return sdk.MemoryDigestRenderer{Source: "builtin:memory-digest", Render: renderDigest, Compose: composeDigest}
}

var kindTitles = map[string]string{
	"profile":    "About the user",
	"preference": "Preferences",
	"rule":       "Rules",
	"reference":  "References",
	"fact":       "Facts",
	"procedure":  "How things are done",
	"episode":    "Recent decisions",
}

func renderDigest(request sdk.MemoryDigestRequest) (sdk.MemoryDigestResult, error) {
	scopes := make(map[string][]sdk.MemoryDigestRecord)
	for _, record := range request.Records {
		scopes[record.Scope] = append(scopes[record.Scope], record)
	}
	result := sdk.MemoryDigestResult{ByScope: make(map[string]string, len(scopes))}
	for scope, records := range scopes {
		budget := request.GlobalBudget
		switch {
		case strings.HasPrefix(scope, "project:"):
			budget = request.ProjectBudget
		case strings.HasPrefix(scope, "agent:"):
			budget = request.AgentBudget
		}
		if text := renderScope(records, request.KindOrder, budget); text != "" {
			result.ByScope[scope] = text
		}
	}
	return result, nil
}

func renderScope(records []sdk.MemoryDigestRecord, order []string, budget int) string {
	slices.SortStableFunc(records, func(left, right sdk.MemoryDigestRecord) int {
		if left.Pinned != right.Pinned {
			if left.Pinned {
				return -1
			}
			return 1
		}
		if diff := kindRank(order, left.Kind) - kindRank(order, right.Kind); diff != 0 {
			return diff
		}
		return right.Updated.Compare(left.Updated)
	})
	picked := map[string][]string{}
	used := 0
	for _, record := range records {
		line := "- " + Indented(record.Text, "  ")
		if record.Why != "" {
			line += " (why: " + strings.Join(strings.Fields(record.Why), " ") + ")"
		}
		size := utf8.RuneCountInString(line) + 1
		if used+size > budget {
			continue
		}
		used += size
		picked[record.Kind] = append(picked[record.Kind], line)
	}
	var result strings.Builder
	for _, kind := range order {
		if len(picked[kind]) == 0 {
			continue
		}
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString(kindTitles[kind] + ":\n")
		result.WriteString(strings.Join(picked[kind], "\n"))
		result.WriteString("\n")
	}
	return strings.TrimSpace(result.String())
}

func composeDigest(request sdk.MemoryDigestDeliveryRequest) (string, error) {
	var parts []string
	used := 0
	add := func(section string) {
		size := utf8.RuneCountInString(section)
		if used+size <= request.Budget {
			used += size
			parts = append(parts, section)
		}
	}
	if request.Global != "" {
		add(request.Global)
	}
	if request.Agent != "" {
		add("Only for you:\n" + request.Agent)
	}
	projects := slices.Clone(request.Projects)
	slices.SortFunc(projects, func(left, right sdk.MemoryDigestProject) int {
		if order := right.Updated.Compare(left.Updated); order != 0 {
			return order
		}
		return strings.Compare(left.Scope, right.Scope)
	})
	for _, project := range projects {
		add("When working in " + project.Location + ":\n" + project.Markdown)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "## Memory\n\nWhat is known from earlier conversations with the user. Follow it unless the user says otherwise now. The project's own instruction files (AGENTS.md and the like) and docs come first: where a memory disagrees with them, follow the project's files and tell the user which memory is out of date. A rule or fact of one project belongs in that project's files; memory keeps what holds for the user across projects. When a memory_search tool is at hand, search it for more before asking the user again, and save what they ask to be remembered with memory_save.\n\n" +
		strings.Join(parts, "\n\n"), nil
}

func kindRank(order []string, kind string) int {
	if i := slices.Index(order, kind); i >= 0 {
		return i
	}
	return len(order)
}

// Indented keeps the line breaks of a Memory item and pads continuation lines.
func Indented(text, pad string) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n")
	for index, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if index > 0 && line != "" {
			line = pad + line
		}
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}
