package memdistill

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/memory"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Dreaming is the nightly tidy: once a day, after DreamHour, a model is
// shown each scope's memories and names the ones that say the same thing
// (merged into one) and the ones no longer true (set aside); candidates
// distilled from conversations and left unreviewed for Unreviewed are set
// aside too. What the user wrote or pinned is never changed
// (memory.Tidy). What the last tidy did is kept in State.Changes, to be
// undone (Undo).

const (
	// DreamHour is the local hour a tidy is due from each day
	DreamHour = 3
	// Unreviewed is how long a distilled candidate waits for review before
	// a tidy sets it aside
	Unreviewed = 30 * 24 * time.Hour
	// perAsk caps the memories one request is shown
	perAsk = 120
)

// SetDream switches the nightly tidy on or off. Switched on, the first is
// due at the next DreamHour; Tidy now does one at once.
func SetDream(on bool) (State, error) {
	return change(func(s *State) {
		if on && !s.Dream {
			s.DreamSince = now().UTC()
		}
		s.Dream = on
	})
}

// Dreams says a tidy is due at t: switched on, and none done since the
// last DreamHour passed (local time), nor since it was switched on.
func Dreams(s State, t time.Time) bool {
	if !s.Dream {
		return false
	}
	t = t.Local()
	at := time.Date(t.Year(), t.Month(), t.Day(), DreamHour, 0, 0, 0, time.Local)
	if t.Before(at) {
		at = at.AddDate(0, 0, -1)
	}
	return s.Dreamt.Before(at) && s.DreamSince.Before(at)
}

// Dream tidies memory now, switched on or not, with the model picked or
// auto. A request that fails changes nothing; the error is kept and the
// tidy is tried again on the next tick.
func Dream(ctx context.Context, auto string, complete Complete) ([]memory.Change, error) {
	return runDream(ctx, func() string { return auto }, complete)
}

func runDream(ctx context.Context, auto func() string, complete Complete) ([]memory.Change, error) {
	dream, err := prepareDream(ctx, auto, complete)
	if err != nil {
		return nil, err
	}
	return dream()
}

func prepareDream(ctx context.Context, auto func() string, complete Complete) (func() ([]memory.Change, error), error) {
	ctx, finish, err := startRun(ctx, dreamRun)
	if err != nil {
		return nil, err
	}
	return func() ([]memory.Change, error) {
		defer finish()
		return dreamOnce(ctx, auto, complete)
	}, nil
}

func dreamOnce(ctx context.Context, auto func() string, complete Complete) ([]memory.Change, error) {
	st, err := Load()
	if err != nil {
		return nil, err
	}
	model := st.Model
	if model == "" && ctx.Err() == nil {
		model = auto()
	}
	var plan memory.Plan
	if err = ctx.Err(); err == nil {
		plan, err = ask(ctx, model, st.Effort, complete)
	}
	if err == nil {
		err = ctx.Err()
	}
	var cs []memory.Change
	if err == nil {
		plan.Before = now().Add(-Unreviewed)
		cs, err = memory.Owner().Tidy(plan)
	}
	_, serr := change(func(s *State) {
		if err != nil {
			s.DreamError = err.Error()
			return
		}
		s.Dreamt, s.DreamModel, s.DreamError, s.Changes, s.Undone = now().UTC(), model, "", cs, false
	})
	return cs, errors.Join(err, serr)
}

// Undo puts back what the last tidy changed, as far as the user hasn't
// changed it since, once.
func Undo() (int, error) {
	if !running.TryLock() {
		return 0, ErrRunning
	}
	defer running.Unlock()
	st, err := Load()
	if err != nil || st.Undone || len(st.Changes) == 0 {
		return 0, err
	}
	n, err := memory.Owner().Untidy(st.Changes)
	if err != nil {
		return n, err
	}
	_, err = change(func(s *State) { s.Undone = true })
	return n, err
}

// ask shows the model each scope's memories that could change, perAsk at
// a time, and gathers what it names. A scope with fewer than two, or with
// only the user's own, isn't sent.
func ask(ctx context.Context, model, effort string, complete Complete) (memory.Plan, error) {
	var p memory.Plan
	codec := linkedDistillationCodec
	all, _, err := memory.Owner().All()
	if err != nil {
		return p, err
	}
	scopes := map[string][]*memory.Memory{}
	var order []string
	for _, m := range all {
		if m.Status != memory.Active && m.Status != memory.Candidate {
			continue
		}
		if _, ok := scopes[m.Scope]; !ok {
			order = append(order, m.Scope)
		}
		scopes[m.Scope] = append(scopes[m.Scope], m)
	}
	for _, sc := range order {
		ms := scopes[sc]
		if len(ms) < 2 || !slices.ContainsFunc(ms, func(m *memory.Memory) bool { return !memory.Fixed(m) }) {
			continue
		}
		slices.SortStableFunc(ms, func(a, b *memory.Memory) int {
			if c := slices.Index(memory.Kinds, a.Kind) - slices.Index(memory.Kinds, b.Kind); c != 0 {
				return c
			}
			return a.Created.Compare(b.Created)
		})
		for i := 0; i < len(ms); i += perAsk {
			chunk := ms[i:min(i+perAsk, len(ms))]
			if len(chunk) < 2 {
				continue
			}
			if model == "" {
				return p, errors.New("no model to tidy memory with: pick one")
			}
			if ctx.Err() != nil {
				return p, ctx.Err()
			}
			prompt := codec.DreamPrompt(sdk.MemoryDreamRequest{Today: now(), Notes: dreamNotes(chunk)})
			reply, err := complete(ctx, model, effort, prompt.System, prompt.User)
			if err != nil {
				return p, fmt.Errorf("%s: %w", model, err)
			}
			if err := ctx.Err(); err != nil {
				return p, err
			}
			mg, st := plan(codec.DreamReply(reply), chunk)
			p.Merges, p.Stale = append(p.Merges, mg...), append(p.Stale, st...)
		}
	}
	return p, nil
}

func dreamNotes(ms []*memory.Memory) []sdk.MemoryDreamNote {
	notes := make([]sdk.MemoryDreamNote, 0, len(ms))
	for i, m := range ms {
		notes = append(notes, sdk.MemoryDreamNote{
			Label: "n" + strconv.Itoa(i+1), Kind: m.Kind, Created: m.Created,
			Uses: m.Uses, Fixed: memory.Fixed(m), Text: m.Text, Why: m.Why,
		})
	}
	return notes
}

func plan(suggestions sdk.MemoryDreamSuggestions, ms []*memory.Memory) ([]memory.Merge, []memory.Stale) {
	id := func(n string) string {
		var k int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "n%d", &k); err != nil || k < 1 || k > len(ms) || fmt.Sprintf("n%d", k) != strings.TrimSpace(n) {
			return ""
		}
		return ms[k-1].ID
	}
	var mg []memory.Merge
	for _, m := range suggestions.Merge {
		var ids []string
		for _, n := range m.Labels {
			if x := id(n); x != "" {
				ids = append(ids, x)
			}
		}
		if len(ids) >= 2 {
			mg = append(mg, memory.Merge{IDs: ids, Text: m.Text, Kind: strings.ToLower(strings.TrimSpace(m.Kind))})
		}
	}
	var st []memory.Stale
	for _, s := range suggestions.Stale {
		if x := id(s.Label); x != "" {
			st = append(st, memory.Stale{ID: x, Why: cut(strings.TrimSpace(s.Why), 200)})
		}
	}
	return mg, st
}
