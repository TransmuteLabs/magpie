package memdistill

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/memory"
)

func note(t *testing.T, text, scope, origin string) *memory.Memory {
	t.Helper()
	k, st := memory.Fact, memory.Active
	if origin == memory.FromInferred {
		st = memory.Candidate
	}
	m, _, err := memory.Save(memory.Input{Text: &text, Kind: &k, Scope: &scope, Status: &st}, origin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func status(t *testing.T, id string) string {
	t.Helper()
	m, err := memory.Get(id)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return m.Status
}

// A tidy shows each scope with something to tidy to the model under short
// names, the user's own marked fixed; what it names is merged or set
// aside, a name it wasn't shown is left out, and the user's own stays as
// written. What it did is kept, undone once.
func TestDreamTidiesAndUndoes(t *testing.T) {
	sb := newSandbox(t)
	a := note(t, "Ships on Tuesdays", memory.Global, memory.FromInferred)
	b := note(t, "Releases go out each Tuesday", memory.Global, memory.FromAgent)
	c := note(t, "Freeze until Nov 1", memory.Global, memory.FromInferred)
	u := note(t, "Use tabs", memory.Global, memory.FromUser)
	p := note(t, "Alone in its project", "project:github.com/a/b", memory.FromInferred)
	var asked []string
	cs, err := Dream(context.Background(), "auto/m", func(_ context.Context, model, _, system, user string) (string, error) {
		asked = append(asked, model+"\n"+user)
		return "Sure:\n```json\n" + `{"merge":[{"ids":["n1","n2","x9"],"text":"Releases ship on Tuesdays","kind":"FACT"}],"stale":[{"id":"n3","why":"date passed"},{"id":"n4"},{"id":"n99"}]}` + "\n```", nil
	})
	if err != nil || len(asked) != 1 || len(cs) != 2 {
		t.Fatalf("%v %d asked, %+v", err, len(asked), cs)
	}
	sent := asked[0]
	if !strings.HasPrefix(sent, "auto/m\n") || !strings.Contains(sent, "n4 | fact | 2026-") || !strings.Contains(sent, "| fixed | Use tabs") ||
		strings.Contains(sent, a.ID) || strings.Contains(sent, "Alone in its project") {
		t.Fatalf("sent:\n%s", sent)
	}
	if status(t, a.ID) != memory.Superseded || status(t, b.ID) != memory.Superseded || status(t, c.ID) != memory.Archived ||
		status(t, u.ID) != memory.Active || status(t, p.ID) != memory.Candidate {
		t.Fatal("wrong ones changed")
	}
	if m, _ := memory.Get(cs[0].Into); m == nil || m.Text != "Releases ship on Tuesdays" || m.Status != memory.Active {
		t.Fatalf("merged: %+v", m)
	}
	st, _ := Load()
	if !st.Dreamt.Equal(sb.at) || st.DreamModel != "auto/m" || len(st.Changes) != 2 || st.Changes[1].Why != "date passed" || st.DreamError != "" {
		t.Fatalf("state: %+v", st)
	}
	if n, err := Undo(); err != nil || n != 3 {
		t.Fatalf("undo: %d %v", n, err)
	}
	if status(t, a.ID) != memory.Candidate || status(t, b.ID) != memory.Active || status(t, c.ID) != memory.Candidate {
		t.Fatal("not undone")
	}
	if n, err := Undo(); err != nil || n != 0 {
		t.Fatalf("undone twice: %d %v", n, err)
	}
}

// A request that fails changes nothing and is kept to show; the tidy is
// due again. With no model there is nothing to ask with, and it says so.
func TestDreamFailureChangesNothing(t *testing.T) {
	newSandbox(t)
	a := note(t, "One", memory.Global, memory.FromInferred)
	note(t, "Two", memory.Global, memory.FromInferred)
	if _, err := SetDream(true); err != nil {
		t.Fatal(err)
	}
	if _, err := Dream(context.Background(), "", nil); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("no model: %v", err)
	}
	_, err := Dream(context.Background(), "a/m", func(context.Context, string, string, string, string) (string, error) {
		return "", errors.New("429: slow down")
	})
	if err == nil || status(t, a.ID) != memory.Candidate {
		t.Fatalf("%v", err)
	}
	st, _ := Load()
	if st.DreamError != "a/m: 429: slow down" || !st.Dreamt.IsZero() || len(st.Changes) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestDreamDiscardsSuccessfulReplyAfterCancel(t *testing.T) {
	newSandbox(t)
	first := note(t, "One", memory.Global, memory.FromInferred)
	second := note(t, "Two", memory.Global, memory.FromInferred)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes, err := Dream(ctx, "a/m", func(context.Context, string, string, string, string) (string, error) {
		cancel()
		return `{"stale":[{"id":"n1","why":"expired"},{"id":"n2","why":"expired"}]}`, nil
	})
	if !errors.Is(err, context.Canceled) || len(changes) != 0 || status(t, first.ID) != memory.Candidate || status(t, second.ID) != memory.Candidate {
		t.Fatalf("canceled tidy applied a late reply: %+v %v", changes, err)
	}
	st, err := Load()
	if err != nil || !st.Dreamt.IsZero() || len(st.Changes) != 0 || st.DreamError != context.Canceled.Error() {
		t.Fatalf("canceled tidy consumed the next run: %+v %v", st, err)
	}
}

func TestDreamNowAdmitsBeforeChoosingModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sb := newSandbox(t)
		first := note(t, "One", memory.Global, memory.FromInferred)
		note(t, "Two", memory.Global, memory.FromInferred)
		release := make(chan struct{})
		choices, requests := 0, 0
		service := Bind(func() string {
			view, err := distillationStatus()
			if err != nil || !view.Dreaming || view.Running {
				t.Errorf("choosing a model outside an admitted tidy: %+v, %v", view, err)
			}
			choices++
			return "a/m"
		}, func(context.Context, string, string, string, string) (string, error) {
			requests++
			<-release
			return `{"stale":[{"id":"n1"}]}`, nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		service.DreamNow(ctx)
		synctest.Wait()
		view, err := service.Status()
		if err != nil || view.Dreaming || !view.Dreamt.IsZero() || view.DreamError != "" || choices != 0 || requests != 0 {
			t.Errorf("canceled admission ran work: %+v, %v, %d choices, %d requests", view, err, choices, requests)
		}

		service.DreamNow(t.Context())
		view, err = service.Status()
		if err != nil || !view.Dreaming || view.Running {
			t.Errorf("manual tidy was not admitted before returning: %+v, %v", view, err)
		}
		synctest.Wait()
		service.DreamNow(t.Context())
		synctest.Wait()
		if choices != 1 || requests != 1 {
			t.Errorf("conflicting tidy repeated work: %d choices, %d requests", choices, requests)
		}
		close(release)
		synctest.Wait()
		view, err = service.Status()
		if err != nil || view.Dreaming || view.Running || !view.Dreamt.Equal(sb.at) || view.DreamModel != "a/m" || status(t, first.ID) != memory.Archived {
			t.Fatalf("completed tidy lost its result: %+v, %v", view, err)
		}
	})
}

// A candidate distilled from a conversation and never reviewed is set
// aside after Unreviewed, with no model asked when nothing else could
// change.
func TestDreamSetsAsideUnreviewed(t *testing.T) {
	sb := newSandbox(t)
	old := sb.at.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	if err := os.WriteFile(memory.Path(), []byte(`{"version":1,"memories":[{"id":"m_old","text":"Old","kind":"fact","scope":"global","origin":"inferred","status":"candidate","created":"`+old+`","updated":"`+old+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Dream(context.Background(), "", nil)
	if err != nil || len(cs) != 1 || cs[0].Why != memory.Unreviewed || status(t, "m_old") != memory.Archived {
		t.Fatalf("%+v %v", cs, err)
	}
}

// A tidy is due once a day from DreamHour local time, never before it
// was switched on.
func TestDreamsOnceANight(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.Local) }
	cases := []struct {
		s    State
		t    time.Time
		want bool
	}{
		{State{}, day(10, 4), false},
		{State{Dream: true, DreamSince: day(9, 14)}, day(10, 2), false},
		{State{Dream: true, DreamSince: day(9, 14)}, day(10, 4), true},
		{State{Dream: true, DreamSince: day(1, 14), Dreamt: day(10, 3).Add(time.Minute)}, day(10, 23), false},
		{State{Dream: true, DreamSince: day(1, 14), Dreamt: day(10, 3).Add(time.Minute)}, day(11, 3), true},
		{State{Dream: true, DreamSince: day(1, 14), Dreamt: day(8, 5)}, day(10, 1), true},
	}
	for i, c := range cases {
		if got := Dreams(c.s, c.t); got != c.want {
			t.Errorf("%d: %v, want %v", i, got, c.want)
		}
	}
}
