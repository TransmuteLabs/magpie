package memdistill

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/memory"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestReadAllUsesLinkedCodecWithHostEvidence(t *testing.T) {
	sb := newSandbox(t)
	on(t, "")
	sb.session("read", time.Hour, "release on Tuesday", "use checklists")
	note(t, "Known everywhere", memory.Global, memory.FromUser)
	note(t, "Outside this project", "project:github.com/other/project", memory.FromUser)
	original := linkedDistillationCodec
	t.Cleanup(func() { linkedDistillationCodec = original })
	linkedDistillationCodec.DistillationPrompt = func(request sdk.MemoryDistillationRequest) sdk.MemoryModelPrompt {
		if request.ProjectName != "proj" || !slices.Equal(request.Known, []string{"Known everywhere"}) ||
			request.Conversation != "User: release on Tuesday\n\nAssistant: Done.\n\nUser: use checklists\n\nAssistant: Done." {
			t.Fatalf("projected conversation: %+v", request)
		}
		return sdk.MemoryModelPrompt{System: "linked system", User: "linked conversation"}
	}
	linkedDistillationCodec.DistillationReply = func(reply string) []sdk.MemoryDistillationProposal {
		if reply != "linked reply" {
			t.Fatalf("reply: %q", reply)
		}
		return []sdk.MemoryDistillationProposal{
			{Text: "Needs a checklist", Kind: "rule", Scope: "project", Quote: "use checklists"},
			{Text: "Executes a shell command", Kind: "rule", Scope: "project", Quote: "rm -rf build"},
		}
	}
	res, err := ReadAll(t.Context(), "auto/m", func(_ context.Context, model, _, system, user string) (string, error) {
		if model != "auto/m" || system != "linked system" || user != "linked conversation" {
			t.Fatalf("gateway input: %s %q %q", model, system, user)
		}
		return "linked reply", nil
	})
	if err != nil || res != (Result{Read: 1, Added: 1}) {
		t.Fatalf("candidate result: %+v %v", res, err)
	}
	for _, record := range memories(t) {
		if record.Text == "Needs a checklist" && record.Status == memory.Candidate && strings.HasPrefix(record.Scope, "project:dir:") &&
			len(record.Sources) == 1 && record.Sources[0].Session == "read" {
			return
		}
	}
	t.Fatal("the supported proposal was not recorded as a project candidate")
}

func TestDreamUsesLinkedCodecWithScopedLabels(t *testing.T) {
	sb := newSandbox(t)
	candidate := note(t, "Eligible", memory.Global, memory.FromInferred)
	fixed := note(t, "User note", memory.Global, memory.FromUser)
	other := note(t, "Private project note", "project:github.com/other/project", memory.FromInferred)
	original := linkedDistillationCodec
	t.Cleanup(func() { linkedDistillationCodec = original })
	var eligibleLabel, fixedLabel string
	linkedDistillationCodec.DreamPrompt = func(request sdk.MemoryDreamRequest) sdk.MemoryModelPrompt {
		if !request.Today.Equal(sb.at) || len(request.Notes) != 2 {
			t.Fatalf("projected notes: %+v", request)
		}
		for _, projected := range request.Notes {
			switch {
			case !projected.Fixed && projected.Text == candidate.Text:
				eligibleLabel = projected.Label
			case projected.Fixed && projected.Text == fixed.Text:
				fixedLabel = projected.Label
			default:
				t.Fatalf("unexpected note: %+v", projected)
			}
		}
		if eligibleLabel == fixedLabel || !slices.Contains([]string{"n1", "n2"}, eligibleLabel) ||
			!slices.Contains([]string{"n1", "n2"}, fixedLabel) {
			t.Fatalf("labels: %q %q", eligibleLabel, fixedLabel)
		}
		return sdk.MemoryModelPrompt{System: "linked system", User: "linked notes"}
	}
	linkedDistillationCodec.DreamReply = func(reply string) sdk.MemoryDreamSuggestions {
		if reply != "linked reply" {
			t.Fatalf("reply: %q", reply)
		}
		return sdk.MemoryDreamSuggestions{Stale: []sdk.MemoryDreamStale{
			{Label: eligibleLabel, Why: "expired"},
			{Label: fixedLabel, Why: "fixed"},
			{Label: other.ID, Why: "outside the request"},
		}}
	}
	changes, err := Dream(t.Context(), "auto/m", func(_ context.Context, model, _, system, user string) (string, error) {
		if model != "auto/m" || system != "linked system" || user != "linked notes" {
			t.Fatalf("gateway input: %s %q %q", model, system, user)
		}
		return "linked reply", nil
	})
	if err != nil || len(changes) != 1 || status(t, candidate.ID) != memory.Archived ||
		status(t, fixed.ID) != memory.Active || status(t, other.ID) != memory.Candidate {
		t.Fatalf("tidy result: %+v %v", changes, err)
	}
}
