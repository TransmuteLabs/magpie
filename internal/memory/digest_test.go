package memory

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// What agents save reaches the digest when it is published, never at the
// save itself, so an agent's instructions don't change mid-session.
func TestDigestChangesOnlyWhenPublished(t *testing.T) {
	at := fresh(t)
	save(t, "Answer in Chinese", Preference, Global)
	if d, _ := Digest("codex"); d != "" {
		t.Fatalf("unpublished digest %q", d)
	}
	if changed, err := Publish(); err != nil || !changed {
		t.Fatalf("publish: %v, %v", changed, err)
	}
	d, _ := Digest("codex")
	if !strings.Contains(d, "Preferences:\n- Answer in Chinese") {
		t.Fatalf("digest %q", d)
	}
	save(t, "Keep commits small", Rule, Global)
	if d2, _ := Digest("codex"); d2 != d {
		t.Fatal("a save changed the digest")
	}
	*at = at.Add(time.Hour)
	if changed, _ := PublishDue(); changed {
		t.Fatal("published again within a day")
	}
	*at = at.Add(24 * time.Hour)
	if changed, _ := PublishDue(); !changed {
		t.Fatal("not published after a day")
	}
	if d, _ := Digest("codex"); !strings.Contains(d, "Rules:\n- Keep commits small") {
		t.Fatalf("digest after a day %q", d)
	}
	if changed, _ := Publish(); changed {
		t.Fatal("an unchanged publish said it changed")
	}
}

func TestPublishUsesLinkedDigestRenderer(t *testing.T) {
	at := fresh(t)
	active := save(t, "Release with a signed tag", Rule, Global)
	archived := save(t, "The previous process", Fact, Global)
	if _, err := Forget(archived.ID); err != nil {
		t.Fatal(err)
	}
	original := linkedDigestRenderer
	t.Cleanup(func() { linkedDigestRenderer = original })
	calls := 0
	renderErr := errors.New("renderer refused the snapshot")
	const rendered = "Rules:\n- Rendered through the linked contract"
	linkedDigestRenderer = sdk.MemoryDigestRenderer{
		Source:  "test:linked-memory-digest",
		Compose: original.Compose,
		Render: func(request sdk.MemoryDigestRequest) (sdk.MemoryDigestResult, error) {
			calls++
			if calls > 1 {
				return sdk.MemoryDigestResult{}, renderErr
			}
			want := sdk.MemoryDigestRecord{Text: active.Text, Kind: Rule, Scope: Global, Updated: active.Updated}
			if !slices.Equal(request.Records, []sdk.MemoryDigestRecord{want}) ||
				!slices.Equal(request.KindOrder, Kinds) || request.GlobalBudget != globalBudget ||
				request.ProjectBudget != projectBudget || request.AgentBudget != agentBudget {
				t.Fatalf("linked request: %+v", request)
			}
			return sdk.MemoryDigestResult{ByScope: map[string]string{Global: rendered}}, nil
		},
	}
	if changed, err := Publish(); err != nil || !changed {
		t.Fatalf("publish: changed %v, error %v", changed, err)
	}
	digest, err := Digest("codex")
	if err != nil || !strings.HasSuffix(digest, rendered) || calls != 1 {
		t.Fatalf("linked digest: %q (%v), render calls %d", digest, err, calls)
	}
	metadata, err := os.ReadFile(in(metaName))
	if err != nil {
		t.Fatal(err)
	}
	*at = at.Add(25 * time.Hour)
	if changed, err := PublishDue(); changed || !errors.Is(err, renderErr) || !strings.Contains(err.Error(), linkedDigestRenderer.Source) {
		t.Fatalf("failed due publish: changed %v, error %v", changed, err)
	}
	if after, err := os.ReadFile(in(metaName)); err != nil || string(after) != string(metadata) {
		t.Fatalf("failed publish changed metadata: %v", err)
	}
	if after, err := Digest("codex"); err != nil || after != digest || calls != 2 {
		t.Fatalf("digest after failure: %q (%v), render calls %d", after, err, calls)
	}
}

func TestDigestUsesLinkedComposer(t *testing.T) {
	fresh(t)
	save(t, "Use the shared build", Rule, Global)
	save(t, "Use the Codex runner", Rule, "agent:codex")
	save(t, "Use the Claude runner", Rule, "agent:claude")
	project := save(t, "Run project checks", Rule, "project:fixture")
	if err := NoteProject(&Project{Key: "fixture", Name: "Fixture", Roots: []string{"/fixture"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(); err != nil {
		t.Fatal(err)
	}
	original := linkedDigestRenderer
	t.Cleanup(func() { linkedDigestRenderer = original })
	linkedDigestRenderer.Compose = func(request sdk.MemoryDigestDeliveryRequest) (string, error) {
		want := sdk.MemoryDigestProject{
			Scope: "project:fixture", Location: "/fixture", Markdown: "Rules:\n- Run project checks", Updated: project.Updated,
		}
		if request.Agent != "Rules:\n- Use the Codex runner" || request.Global != "Rules:\n- Use the shared build" ||
			request.Budget != allBudget || !slices.Equal(request.Projects, []sdk.MemoryDigestProject{want}) {
			t.Fatalf("unexpected delivery projection: %+v", request)
		}
		return "linked instructions", nil
	}
	if got, err := Digest("codex"); got != "linked instructions" || err != nil {
		t.Fatalf("ordinary digest bypassed the linked composer: %q (%v)", got, err)
	}
	refused := errors.New("compose failed")
	linkedDigestRenderer.Compose = func(sdk.MemoryDigestDeliveryRequest) (string, error) {
		return "partial", refused
	}
	if got, err := Digest("codex"); got != "" || !errors.Is(err, refused) {
		t.Fatalf("failed compose returned %q (%v)", got, err)
	}
}

func TestDigestPartsAndBudget(t *testing.T) {
	at := fresh(t)
	old := globalBudget
	globalBudget = 120
	t.Cleanup(func() { globalBudget = old })

	save(t, strings.Repeat("a", 60), Fact, Global)
	*at = at.Add(time.Minute)
	save(t, strings.Repeat("b", 60), Fact, Global)
	pin := save(t, "pinned one", Episode, Global)
	tr := true
	if _, err := Edit(pin.ID, Input{Pinned: &tr}, true); err != nil {
		t.Fatal(err)
	}
	save(t, "only codex sees this", Rule, "agent:codex")
	p := &Project{Key: "github.com/yetone/magpie", Name: "magpie", Roots: []string{"/w/magpie"}}
	if _, _, err := Save(Input{Text: str("tests run on mote"), Kind: str(Rule), Scope: str(ScopeOf(p))}, FromAgent, nil, p); err != nil {
		t.Fatal(err)
	}
	gone := save(t, "archived one", Rule, Global)
	if _, err := Forget(gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(); err != nil {
		t.Fatal(err)
	}
	d, _ := Digest("codex")
	for _, want := range []string{"pinned one", strings.Repeat("b", 60), "Only for you:\n" + "Rules:\n- only codex sees this", "When working in /w/magpie:\nRules:\n- tests run on mote"} {
		if !strings.Contains(d, want) {
			t.Errorf("digest lacks %q:\n%s", want, d)
		}
	}
	// over budget: the older of the two long ones is left out, whole
	for _, not := range []string{strings.Repeat("a", 60), "archived one"} {
		if strings.Contains(d, not) {
			t.Errorf("digest has %q:\n%s", not, d)
		}
	}
	if other, _ := Digest("claude"); strings.Contains(other, "only codex") {
		t.Errorf("another agent got codex's own:\n%s", other)
	}
	for _, name := range []string{"magpie", "Magpie", "OpenAI", "Anthropic"} {
		if strings.Contains(strings.SplitN(d, "\n\n", 3)[1], name) {
			t.Errorf("the digest's header names %s", name)
		}
	}
}

// The digest defers to the project's own instruction files: a memory that
// disagrees with them loses, and a project's rules are pointed to its
// files (flying_crp on X: memory and AGENTS.md said opposite things, and
// a project's rule went into memory). The header names neither this app
// nor a vendor.
func TestDigestDefersToTheProjectsFiles(t *testing.T) {
	fresh(t)
	save(t, "Answer in Chinese", Preference, Global)
	if _, err := Publish(); err != nil {
		t.Fatal(err)
	}
	d, _ := Digest("codex")
	head := strings.SplitN(d, "\n\n", 3)[1]
	for _, want := range []string{
		"The project's own instruction files (AGENTS.md and the like) and docs come first",
		"where a memory disagrees with them, follow the project's files",
		"A rule or fact of one project belongs in that project's files",
		"memory keeps what holds for the user across projects",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the header lacks %q:\n%s", want, head)
		}
	}
	for _, name := range []string{"magpie", "claude", "codex", "openai", "anthropic", "gemini"} {
		if strings.Contains(strings.ToLower(head), name) {
			t.Errorf("the header names %s", name)
		}
	}
}

// A memory the user writes in several lines keeps them: on save, on an
// edit, in its file, and in the digest, where the lines after the first
// are indented under its item (_meouwu on X: the line breaks were gone).
func TestMultiLineMemoryKeepsItsLines(t *testing.T) {
	fresh(t)
	text := "Release steps:\n1. tag the commit\n2. push the tag by name\n\n   - never --tags"
	m, _, err := Save(Input{Text: str(text + "\n"), Kind: str(Procedure), Scope: str(Global)}, FromUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != text {
		t.Fatalf("saved %q, want %q", m.Text, text)
	}
	text += "\n3. wait for the assets"
	if _, err := Edit(m.ID, Input{Text: str(text)}, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(in(m.ID + ".md"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decode(m.ID, b, time.Now()); err != nil || got.Text != text {
		t.Fatalf("the file reads %q (%v), want %q", got.Text, err, text)
	}
	if got, _ := Get(m.ID); got.Text != text {
		t.Fatalf("Get %q, want %q", got.Text, text)
	}
	if _, err := Publish(); err != nil {
		t.Fatal(err)
	}
	d, _ := Digest("codex")
	want := "How things are done:\n- Release steps:\n  1. tag the commit\n  2. push the tag by name\n\n     - never --tags\n  3. wait for the assets"
	if !strings.Contains(d, want) {
		t.Fatalf("digest %q, want it to hold %q", d, want)
	}
}

func TestIndented(t *testing.T) {
	for in, want := range map[string]string{
		"one line":                  "one line",
		"  a\r\nb  \n\n\tc\n":       "a\n> b\n\n> \tc",
		"a \t\n   \nb":              "a\n\n> b",
		"\n\nonly after blanks\n\n": "only after blanks",
	} {
		if got := Indented(in, "> "); got != want {
			t.Errorf("Indented(%q) = %q, want %q", in, got, want)
		}
	}
}
