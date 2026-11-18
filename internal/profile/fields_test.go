package profile

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agent"
)

func TestProfileFieldIdentityNeighbors(t *testing.T) {
	sandbox(t)
	for _, id := range []string{"native", "sdk.agent", "codex@wsl:Ubuntu-24.04"} {
		t.Run(id, func(t *testing.T) {
			a := &agent.Agent{ID: id, Name: id, Fields: []agent.Field{{Key: "model", Label: "Model", Get: func() string { return "saved" }}}}
			for _, key := range []string{id + ".model", fieldKey(id, "model")} {
				p := Profile{Fields: map[string]string{key: "saved"}}
				body, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				var loaded Profile
				if err := json.Unmarshal(body, &loaded); err != nil {
					t.Fatal(err)
				}
				if !InUse(loaded, []*agent.Agent{a}, nil) {
					t.Fatalf("legacy/explicit identity not in use: %s", key)
				}
				groups := DetailsIn(loaded, []*agent.Agent{a})
				if len(groups) != 1 || groups[0].ID != id || len(groups[0].Fields) != 1 || groups[0].Fields[0].Key != "model" {
					t.Fatalf("identity changed in details: %+v", groups)
				}
				if got := Summary(loaded); got != id+" saved" {
					t.Fatalf("summary=%q", got)
				}
			}
		})
	}
	if fieldKey("claude", "model") != "claude.model" {
		t.Fatal("ordinary native profile format changed")
	}
	p := Profile{Fields: map[string]string{"gone.agent.model": "saved"}}
	groups := DetailsIn(p, nil)
	if len(groups) != 1 || groups[0].ID != "gone.agent" || InUse(p, nil, nil) {
		t.Fatalf("missing legacy agent lost its display/skipping behavior: %+v", groups)
	}
}

func TestProfileFieldAliasesCannotHideConflictingValues(t *testing.T) {
	sandbox(t)
	a := &agent.Agent{ID: "sdk.agent", Name: "SDK agent", Fields: []agent.Field{{Key: "model", Label: "Model", Get: func() string { return "second" }}}}
	p := Profile{Fields: map[string]string{"sdk.agent.model": "first", fieldKey("sdk.agent", "model"): "second"}}
	if InUse(p, []*agent.Agent{a}, nil) {
		t.Fatal("conflicting aliases marked in use")
	}
	groups := DetailsIn(p, []*agent.Agent{a})
	if len(groups) != 2 {
		t.Fatalf("conflicting alias hidden: %+v", groups)
	}
	for _, group := range groups {
		if !strings.Contains(group.Name, "conflicting") || group.ID == a.ID {
			t.Fatalf("details chose a conflicting value: %+v", group)
		}
	}
	if n, err := ApplyFields(p.Fields); err == nil || n != 0 {
		t.Fatalf("conflicting aliases accepted: %d %v", n, err)
	}
}

func TestProfileInvalidExplicitFieldFailsClosedAndRedactsDetails(t *testing.T) {
	sandbox(t)
	p := Profile{Fields: map[string]string{`@field:["agent"]`: "sk-sensitive-value"}}
	if n, err := ApplyFields(p.Fields); err == nil || n != 0 {
		t.Fatalf("invalid explicit key accepted: %d %v", n, err)
	}
	if InUse(p, nil, nil) {
		t.Fatal("invalid explicit field in use")
	}
	groups := DetailsIn(p, nil)
	if len(groups) != 1 || len(groups[0].Fields) != 1 || !groups[0].Fields[0].Hidden || groups[0].Fields[0].Value != "" {
		t.Fatalf("unresolved field was not redacted: %+v", groups)
	}
}
