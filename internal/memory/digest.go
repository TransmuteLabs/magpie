package memory

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/extension/builtin/memorydigest"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// The digest is the part of memory an agent is given in its instructions,
// so it knows the user's preferences and rules from its first word, before
// it thinks to search. It is held to a budget, pinned memories and the
// kinds that hold longest first, and changes only when Publish is called:
// when the user changes memory, and once a day for what agents saved
// meanwhile. An agent's instructions change mid-session only then, so the
// prompt caches built on them aren't thrown away at every save.
//
// Its text goes to every agent's model vendor: it names neither magpie nor
// any vendor, only the memories and how to read them.

// Budgets, in characters, of the global part and of each project's.
var (
	globalBudget  = 4000
	projectBudget = 1500
	agentBudget   = 1000
	allBudget     = 8000
)

var linkedDigestRenderer = memorydigest.Renderer()

func Indented(s, pad string) string {
	return memorydigest.Indented(s, pad)
}

func digestRequest(f *file) sdk.MemoryDigestRequest {
	request := sdk.MemoryDigestRequest{
		Records:       make([]sdk.MemoryDigestRecord, 0, len(f.Memories)),
		KindOrder:     slices.Clone(Kinds),
		GlobalBudget:  globalBudget,
		ProjectBudget: projectBudget,
		AgentBudget:   agentBudget,
	}
	for _, m := range f.Memories {
		if m.Status == Active {
			request.Records = append(request.Records, sdk.MemoryDigestRecord{
				Text: m.Text, Why: m.Why, Kind: m.Kind, Scope: m.Scope, Pinned: m.Pinned, Updated: m.Updated,
			})
		}
	}
	return request
}

// Publish makes what memory holds now the digest agents are given. It
// reports whether any digest changed, for the caller to write it into the
// agents' instructions.
func Publish() (changed bool, err error) {
	return publish(false)
}

// PublishDue publishes when the digest was last published more than a day
// ago, and memory holds something or held something then. It reports
// whether any digest changed.
func PublishDue() (bool, error) {
	return publish(true)
}

func publish(due bool) (changed bool, err error) {
	err = change(func(f *file) error {
		at := now()
		if due && at.Sub(f.Published) < 24*time.Hour {
			return errSkip
		}
		if len(f.broken) != 0 {
			return errors.New("Memory digest snapshot has unreadable records")
		}
		if due && len(f.Memories) == 0 && len(f.Digest) == 0 {
			return errSkip
		}
		renderer := linkedDigestRenderer
		request := digestRequest(f)
		scopes := make(map[string]bool)
		for _, record := range request.Records {
			scopes[record.Scope] = true
		}
		result, err := renderer.Render(request)
		if err != nil {
			return fmt.Errorf("render Memory digest with %s: %w", renderer.Source, err)
		}
		for scope := range result.ByScope {
			if !scopes[scope] {
				return fmt.Errorf("render Memory digest with %s: unselected scope %q", renderer.Source, scope)
			}
		}
		changed = !maps.Equal(f.Digest, result.ByScope)
		f.Digest = result.ByScope
		f.Published = at
		return nil
	})
	if errors.Is(err, errSkip) {
		return false, nil
	}
	return changed && err == nil, err
}

// Digest is the published digest an agent is given: the global part, the
// agent's own, and each project's under the folders it was seen in, held
// to the whole budget, the projects with the newest memories first. ""
// when there is nothing, or memory can't be read (unknown: the
// caller keeps what the agent has).
func Digest(agent string) (string, error) {
	f, err := view()
	if err != nil {
		return "", err
	}
	if len(f.Digest) == 0 {
		return "", nil
	}
	renderer := linkedDigestRenderer
	content, err := renderer.Compose(digestDelivery(f, agent))
	if err != nil {
		return "", fmt.Errorf("compose Memory digest with %s: %w", renderer.Source, err)
	}
	return content, nil
}

func digestDelivery(f *file, agent string) sdk.MemoryDigestDeliveryRequest {
	request := sdk.MemoryDigestDeliveryRequest{Global: f.Digest[Global], Budget: allBudget}
	if agent != "" {
		request.Agent = f.Digest["agent:"+agent]
	}
	latest := map[string]time.Time{}
	for _, memory := range f.Memories {
		if memory.Status == Active && strings.HasPrefix(memory.Scope, "project:") && memory.Updated.After(latest[memory.Scope]) {
			latest[memory.Scope] = memory.Updated
		}
	}
	for scope, markdown := range f.Digest {
		key, project := strings.CutPrefix(scope, "project:")
		if !project {
			continue
		}
		location := key
		if entry := f.Projects[key]; entry != nil && len(entry.Roots) > 0 {
			location = strings.Join(entry.Roots, ", ")
		}
		request.Projects = append(request.Projects, sdk.MemoryDigestProject{
			Scope: scope, Location: location, Markdown: markdown, Updated: latest[scope],
		})
	}
	return request
}
