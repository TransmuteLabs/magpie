package memorydistillation

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Codec() sdk.MemoryDistillationCodec {
	return sdk.MemoryDistillationCodec{
		Source:             "builtin:memory-distillation",
		DistillationPrompt: distillationPrompt,
		DistillationReply:  distillationReply,
		DreamPrompt:        dreamPrompt,
		DreamReply:         dreamReply,
	}
}

func distillationPrompt(request sdk.MemoryDistillationRequest) sdk.MemoryModelPrompt {
	var user strings.Builder
	if request.ProjectName != "" {
		fmt.Fprintf(&user, "Project: %s\n\n", request.ProjectName)
	}
	user.WriteString("Already known:\n")
	if len(request.Known) == 0 {
		user.WriteString("(nothing)\n")
	}
	for _, text := range request.Known {
		user.WriteString("- " + oneLine(text) + "\n")
	}
	user.WriteString("\nConversation:\n\n")
	user.WriteString(request.Conversation)
	return sdk.MemoryModelPrompt{System: distillationInstructions, User: user.String()}
}

func distillationReply(reply string) []sdk.MemoryDistillationProposal {
	first, last := strings.Index(reply, "["), strings.LastIndex(reply, "]")
	if first < 0 || last < first {
		return nil
	}
	var proposals []sdk.MemoryDistillationProposal
	if json.Unmarshal([]byte(reply[first:last+1]), &proposals) != nil {
		return nil
	}
	return proposals
}

func dreamPrompt(request sdk.MemoryDreamRequest) sdk.MemoryModelPrompt {
	var user strings.Builder
	fmt.Fprintf(&user, "Today: %s\n\nNotes:\n", request.Today.Format("2006-01-02"))
	for _, note := range request.Notes {
		fmt.Fprintf(&user, "%s | %s | %s | used %d× | ", note.Label, note.Kind, note.Created.Local().Format("2006-01-02"), note.Uses)
		if note.Fixed {
			user.WriteString("fixed | ")
		}
		user.WriteString(oneLine(note.Text))
		if note.Why != "" {
			user.WriteString(" — " + oneLine(note.Why))
		}
		user.WriteString("\n")
	}
	return sdk.MemoryModelPrompt{System: dreamInstructions, User: user.String()}
}

func dreamReply(reply string) sdk.MemoryDreamSuggestions {
	first, last := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if first < 0 || last < first {
		return sdk.MemoryDreamSuggestions{}
	}
	var suggestions sdk.MemoryDreamSuggestions
	if json.Unmarshal([]byte(reply[first:last+1]), &suggestions) != nil {
		return sdk.MemoryDreamSuggestions{}
	}
	return suggestions
}

func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

const distillationInstructions = `You read a conversation between a user and a coding assistant and write down what is worth remembering in later conversations, with this user or in this project.

Keep only what stays true beyond this conversation and that a later assistant couldn't learn by reading the code:
- profile: who the user is (role, languages they speak, what they know)
- preference: how the user wants things done, stated or shown by a correction
- rule: a rule learned from a mistake the user corrected, with why
- fact: something true of the project that the code doesn't say
- reference: where something is (a host, a dashboard, a URL, a document)
- procedure: steps that worked for a task likely to come again
- episode: a decision that was made, and why

Each item rests on words the user wrote in this conversation: quote them exactly. Leave out what only the assistant said, such as rules it follows from its own instructions or notes.

A rule or convention of one project belongs in the project's own instruction files and docs, which the user keeps up to date, not here: leave it out unless it is about how this user works everywhere. Never keep an item that disagrees with what the project's files say in the conversation.

Leave out: the task itself and its progress, anything about this conversation alone, code details visible in the repository, what the project's instruction files or docs say, secrets (keys, tokens, passwords), and anything already in "Already known". When nothing is worth keeping, answer [].

Answer with a JSON array alone, at most 5 items:
[{"text": "one short sentence, in the language the user wrote in", "kind": "preference", "scope": "global or project", "why": "what in the conversation shows it", "quote": "the user's own words that show it, copied exactly, short"}]
scope is "project" when it holds only in this project, "global" when it holds for the user everywhere.`

const dreamInstructions = `You tidy a list of notes kept about a user and their projects, for a coding assistant to read in later conversations. Each line is: name | kind | date written | how often it was used | "fixed" when the user wrote or pinned it | text — why.

Find two things:
- merge: notes that say the same thing, or where one says all another does. Give their names and one text that keeps everything they say, short, in the language they are written in, and its kind. A fixed note's words can't change: when one is in a merge, it is the note kept, and the others go into it.
- stale: notes no longer true: one a later note contradicts, one about a date that has passed, one about a one-off task that is done. A decision, a direction or a preference stays even when the work it came from is done. Never a fixed note.

When unsure, leave a note as it is. Most notes stay.

Answer with a JSON object alone:
{"merge": [{"ids": ["n1", "n4"], "text": "the merged note", "kind": "preference"}], "stale": [{"id": "n7", "why": "a few words"}]}
When there is nothing to do, answer {"merge": [], "stale": []}.`
