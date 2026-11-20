// Package memdistill reads the agents' finished conversations and asks a
// model what in them is worth remembering: a preference the user stated,
// a rule learned from a correction, a fact of the project the code
// doesn't say. What it finds waits in memory as a candidate (To review)
// until the user keeps it; nothing it proposes reaches an agent before.
//
// It runs when switched on (State.On), on sessions quiet for Quiet, each
// once for what it held when read: a session that goes on is read again
// later, whole. The hourly run reads at most PerRun; one the user asked
// for (ReadAll) reads every one due, and Stop ends it. The model is the one
// picked, else the caller's automatic choice. The request goes through
// magpie's own gateway (Complete), so masking and routing apply as to any
// agent's request.
package memdistill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension/builtin/memorydistillation"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/memory"
	"github.com/yetone/magpie/internal/sessions"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var linkedDistillationCodec = memorydistillation.Codec()

// Complete sends one request to model, asked to reason at effort ("" the
// model's own default): a system text and one user message, and returns
// the reply's text.
type Complete func(ctx context.Context, model, effort, system, user string) (string, error)

const (
	// Quiet is how long a session has had no turn before it is read: one
	// still going is read when it stops
	Quiet = 30 * time.Minute
	// PerRun caps the sessions an hourly run sends to the model: more
	// than a busy hour brings, so only a backlog waits for the next run.
	// A run the user asked for (ReadAll) reads every one due
	// (RealZeroP27 on X: at 10 an hour, 400 took 40 hours)
	PerRun = 30
	// Back is how far back a run looks the first time it is switched on,
	// when no range is picked (State.Days 0)
	Back = 7 * 24 * time.Hour
	// All is State.Days for every conversation listed: the latest
	// Listed, read at once when picked (ReadAll), else PerRun an hour
	All = -1
	// Listed is how many of the latest sessions a run looks through
	Listed = 400
	// MinTurns is the fewest things the user said for a session to be read
	MinTurns = 2
	// MaxPerSession caps what one session may propose
	MaxPerSession = 5
	// budget is the transcript's size sent, in characters
	budget = 24_000
	// keepMarks is how long a read session's mark is kept
	keepMarks = 30 * 24 * time.Hour
)

// Agent is the agent id the model requests are made as (gateway.TokenFor),
// so Usage counts them apart, and whose sessions are never read.
const Agent = "memory"

// State is distill.json.
type State struct {
	On bool `json:"on,omitempty"`
	// Model is the model picked; "" is the automatic choice
	Model string `json:"model,omitempty"`
	// Days is how far back reading looks, in days, from when it was
	// switched on or the range was picked: 0 is Back, All every
	// conversation listed (Ranges)
	Days int `json:"days,omitempty"`
	// Effort is the reasoning level the model is asked at, reading and
	// tidying, fitted to its own levels as it is sent; "" its default
	Effort string `json:"effort,omitempty"`
	// Since is when it was switched on or the range picked, less the
	// range: older sessions aren't read. Zero with All
	Since time.Time `json:"since,omitzero"`
	// Done is each read session's last turn when it was read, by
	// agent/id
	Done map[string]time.Time `json:"done,omitempty"`
	// the last run: when, with which model, how many sessions read, how
	// many memories proposed, and what stopped it
	Last      time.Time `json:"last,omitzero"`
	LastModel string    `json:"lastModel,omitempty"`
	Read      int       `json:"read,omitempty"`
	Added     int       `json:"added,omitempty"`
	Error     string    `json:"error,omitempty"`
	// Left is how many were due and not read when the last run ended:
	// stopped, failed or past PerRun
	Left int `json:"left,omitempty"`
	// Dream is the nightly tidy switched on (dream.go), DreamSince when
	Dream      bool      `json:"dream,omitempty"`
	DreamSince time.Time `json:"dreamSince,omitzero"`
	// the last tidy that went through: when, with which model, and what it
	// changed, undone or not; DreamError is what stopped the last one tried
	Dreamt     time.Time       `json:"dreamt,omitzero"`
	DreamModel string          `json:"dreamModel,omitempty"`
	DreamError string          `json:"dreamError,omitempty"`
	Changes    []memory.Change `json:"changes,omitempty"`
	Undone     bool            `json:"undone,omitempty"`
}

// Path is distill.json; a variable for tests.
var Path = func() string { return appdir.Join(appdir.Config(), "distill.json") }

var (
	now  = time.Now
	list = func() []sessions.Session { return sessions.List(Listed) }
	read = sessions.TranscriptOf
	// running is held through a run in this process, and the file lock
	// across magpies
	running sync.Mutex
	busy    atomic.Bool
	// stop ends the run going (Stop), stopped says it was asked to
	stopMu  sync.Mutex
	stop    context.CancelFunc
	stopped atomic.Bool
	// doing and of are the run's progress: sessions gone through, of
	// how many
	doing, of atomic.Int64
)

// Running says a run is going in this process.
func Running() bool { return busy.Load() }

// Progress is how many sessions the run going has gone through, of how
// many it will, read or passed over; 0, 0 when none is going or it is
// still listing them.
func Progress() (int, int) { return int(doing.Load()), int(of.Load()) }

// Stop ends the run going in this process, if any: the request in flight
// is dropped and its session read next time. It isn't an error.
func Stop() {
	stopMu.Lock()
	defer stopMu.Unlock()
	if stop != nil {
		stopped.Store(true)
		stop()
	}
}

// ErrRunning is a run asked for while one is going.
var ErrRunning = errors.New("already reading conversations")

// Load reads distill.json; a missing file is the default (off). One that
// can't be read is an error, never the default: a run doesn't start, and
// a change isn't written over it.
func Load() (State, error) {
	var s State
	b, err := lastgood.Read(Path(), lastgood.Into[State])
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}

func save(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	return lastgood.Keep(p, lastgood.Into[State], 0o600)
}

// change applies fn to distill.json under its lock.
func change(fn func(*State)) (State, error) {
	return changeState(fn, false)
}

func changeState(fn func(*State), stopAfter bool) (State, error) {
	unlock, held := filelock.Lock(Path()+".lock", 10*time.Second)
	if !held {
		return State{}, errors.New("distill.json is held by another magpie")
	}
	defer unlock()
	s, err := Load()
	if err != nil {
		return s, err
	}
	fn(&s)
	if err := save(s); err != nil {
		return s, err
	}
	if stopAfter {
		Stop()
	}
	return s, nil
}

// Ranges are the ranges, in days, reading can be set to look back: Back's
// seven first, All last.
var Ranges = []int{7, 30, 90, 365, All}

// since is the oldest a session read may be when days is picked at t.
func since(days int, t time.Time) time.Time {
	switch days {
	case All:
		return time.Time{}
	case 0:
		return t.Add(-Back).UTC()
	}
	return t.Add(-time.Duration(days) * 24 * time.Hour).UTC()
}

// Set switches distilling on or off and picks its model ("" automatic).
// Switched on from off, it looks back the range picked (Back by default)
// from now; switched off, the run going stops.
func Set(on bool, model string) (State, error) {
	state, _, err := Configure(SettingsChange{On: on, Model: model})
	return state, err
}

type SettingsChange struct {
	On     bool    `json:"on"`
	Model  string  `json:"model"`
	Effort *string `json:"effort"`
	Days   *int    `json:"days"`
}

func Configure(in SettingsChange) (State, bool, error) {
	var effort string
	if in.Effort != nil {
		level, err := normalizedEffort(*in.Effort)
		if err != nil {
			return State{}, false, err
		}
		effort = level
	}
	var days int
	if in.Days != nil {
		rangeDays, err := normalizedRange(*in.Days)
		if err != nil {
			return State{}, false, err
		}
		days = rangeDays
	}
	model := strings.TrimSpace(in.Model)
	read := false
	state, err := changeState(func(s *State) {
		longer := false
		if in.Days != nil {
			lookback := since(days, now())
			longer = lookback.Before(s.Since)
			s.Days, s.Since = days, lookback
		} else if in.On && !s.On {
			s.Since = since(s.Days, now())
		}
		if in.Effort != nil {
			s.Effort = effort
		}
		read = in.On && (!s.On || s.Model != model || longer)
		s.On, s.Model = in.On, model
	}, !in.On)
	return state, read && err == nil, err
}

// SetRange picks how far back reading looks: one of Ranges, from now. A
// conversation read already isn't read again for a longer range; one
// older than a shorter range isn't read.
func SetRange(days int) (State, error) {
	days, err := normalizedRange(days)
	if err != nil {
		return State{}, err
	}
	return change(func(s *State) {
		s.Days, s.Since = days, since(days, now())
	})
}

func normalizedRange(days int) (int, error) {
	if !slices.Contains(Ranges, days) {
		return 0, fmt.Errorf("a range of %v days, not %d", Ranges, days)
	}
	if days == 7 {
		days = 0 // Back, kept as before a range could be picked
	}
	return days, nil
}

// SetEffort picks the reasoning level the model is asked at, "" for its
// own default. A level the model doesn't have is fitted to the nearest it
// has when sent.
func SetEffort(effort string) (State, error) {
	effort, err := normalizedEffort(effort)
	if err != nil {
		return State{}, err
	}
	return change(func(s *State) { s.Effort = effort })
}

func normalizedEffort(effort string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort != "" && !slices.Contains(efforts, effort) {
		return "", fmt.Errorf("a reasoning level of %v, not %q", efforts, effort)
	}
	return effort, nil
}

// efforts are the levels SetEffort takes (gateway's effortRank).
var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Result is what one run did.
type Result struct {
	Read  int
	Added int
}

// Run reads up to PerRun of the sessions due, if switched on, with auto
// when the state names no model. A model that fails stops the run: the
// sessions not read yet are read next time.
func Run(ctx context.Context, auto string, complete Complete) (Result, error) {
	return runUpTo(ctx, auto, complete, PerRun)
}

// ReadAll is Run over every session due, for a run the user asked for:
// switched on, a model or a longer range picked, Read now. Stop ends it.
func ReadAll(ctx context.Context, auto string, complete Complete) (Result, error) {
	return runUpTo(ctx, auto, complete, Listed)
}

func runUpTo(ctx context.Context, auto string, complete Complete, limit int) (Result, error) {
	if !running.TryLock() {
		return Result{}, ErrRunning
	}
	defer running.Unlock()
	busy.Store(true)
	defer busy.Store(false)
	unlock, held := filelock.Lock(Path()+".run", 0)
	if !held {
		return Result{}, ErrRunning
	}
	defer unlock()
	st, err := Load()
	if err != nil || !st.On {
		return Result{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stopMu.Lock()
	stop = cancel
	stopped.Store(false)
	stopMu.Unlock()
	defer func() {
		stopMu.Lock()
		stop = nil
		stopMu.Unlock()
		cancel()
		doing.Store(0)
		of.Store(0)
	}()
	model := st.Model
	if model == "" {
		model = auto
	}
	var res ran
	var runErr error
	if model == "" {
		runErr = errors.New("no model to read conversations with: pick one")
	} else {
		res, runErr = run(ctx, st, model, st.Effort, complete, limit)
	}
	if stopped.Load() && errors.Is(runErr, context.Canceled) {
		runErr = nil
	}
	_, err = change(func(s *State) {
		s.Last, s.LastModel, s.Read, s.Added, s.Error, s.Left = now().UTC(), model, res.Read, res.Added, "", res.left
		if runErr != nil {
			s.Error = runErr.Error()
		}
		if s.Done == nil {
			s.Done = map[string]time.Time{}
		}
		for k, at := range res.done {
			s.Done[k] = at
		}
		for k, at := range s.Done {
			if forget(at, s.Since, res.oldest) {
				delete(s.Done, k)
			}
		}
	})
	return res.Result, errors.Join(runErr, err)
}

type ran struct {
	Result
	done map[string]time.Time
	// oldest is the last turn of the oldest session listed, zero when
	// none was
	oldest time.Time
	// left is how many due weren't gone through
	left int
}

// forget says a read session's mark, its last turn at, can go: the
// session is older than since, so it is never read again, or older than
// keepMarks and than every session listed (oldest), so it was deleted or
// fell off the listing, which it comes back to only with a turn after
// the mark. A mark kept only keepMarks would have a session older than
// that, inside the range, read again every hour.
func forget(at, since, oldest time.Time) bool {
	return at.Before(since) || now().Sub(at) > keepMarks && !oldest.IsZero() && at.Before(oldest)
}

// due are the sessions to read now, the latest first, and the last turn
// of the oldest listed.
func due(st State, t time.Time) ([]sessions.Session, time.Time) {
	var out []sessions.Session
	var oldest time.Time
	for _, s := range list() {
		if oldest.IsZero() || s.Last.Before(oldest) {
			oldest = s.Last
		}
		switch {
		case s.Agent == Agent, s.Gateway, s.ReadOnly, !s.Transcript,
			s.ParentSession != "",
			t.Sub(s.Last) < Quiet, s.Last.Before(st.Since):
			continue
		}
		if at, ok := st.Done[key(s)]; ok && !s.Last.After(at) {
			continue
		}
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b sessions.Session) int { return b.Last.Compare(a.Last) })
	return out, oldest
}

func key(s sessions.Session) string { return s.Agent + "/" + s.ID }

func run(ctx context.Context, st State, model, effort string, complete Complete, limit int) (ran, error) {
	r := ran{done: map[string]time.Time{}}
	ss, oldest := due(st, now())
	r.oldest = oldest
	r.left = len(ss)
	of.Store(int64(len(ss)))
	for _, s := range ss {
		if r.Read >= limit {
			break
		}
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		r.left--
		doing.Add(1)
		tr, err := read(s)
		if err != nil {
			// one that can't be read now is tried again when it changes
			r.done[key(s)] = s.Last
			continue
		}
		text, turns := transcript(tr)
		if turns < MinTurns {
			r.done[key(s)] = s.Last
			continue
		}
		proj := memory.ProjectOf(s.Cwd)
		codec := linkedDistillationCodec
		request := sdk.MemoryDistillationRequest{Conversation: text, Known: known(proj)}
		if proj != nil {
			request.ProjectName = proj.Name
		}
		prompt := codec.DistillationPrompt(request)
		reply, err := complete(ctx, model, effort, prompt.System, prompt.User)
		if canceled := ctx.Err(); canceled != nil {
			r.left++
			return r, canceled
		}
		if err != nil {
			r.left++ // read again next time
			return r, fmt.Errorf("%s: %w", model, err)
		}
		r.Read++
		r.done[key(s)] = s.Last
		r.Added += keep(codec.DistillationReply(reply), s, proj, said(tr))
	}
	return r, nil
}

// transcript is what was said, user and assistant text alone (no tool
// calls, results or thinking), each turn named, within budget: the start
// and the end kept when it is longer. turns counts the user's.
func transcript(t sessions.Transcript) (string, int) {
	var lines []string
	turns := 0
	for _, p := range t.Parts {
		if p.Kind != "text" || p.Role != "user" && p.Role != "assistant" {
			continue
		}
		who, max := "User", 4000
		if p.Role == "assistant" {
			who, max = "Assistant", 1200
		} else {
			turns++
		}
		lines = append(lines, who+": "+cut(p.Text, max))
	}
	s := strings.Join(lines, "\n\n")
	if len(s) > budget {
		head, tail := budget/3, budget-budget/3
		s = cutBytes(s[:head]) + "\n\n[…]\n\n" + strings.ToValidUTF8(s[len(s)-tail:], "")
	}
	return s, turns
}

func cut(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func cutBytes(s string) string { return strings.ToValidUTF8(s, "") }

// known are the memories the model is shown, so it proposes none of them
// again: the active and waiting ones everyone has, and the project's.
func known(proj *memory.Project) []string {
	ms, _, err := memory.Owner().All()
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range ms {
		if m.Status != memory.Active && m.Status != memory.Candidate {
			continue
		}
		if m.Scope == memory.Global || proj != nil && m.Scope == "project:"+proj.Key {
			out = append(out, m.Text)
		}
	}
	if len(out) > 80 {
		out = out[len(out)-80:]
	}
	return out
}

// said is everything the user wrote, as letters and digits alone, for
// telling a quote of theirs from words they never wrote.
func said(t sessions.Transcript) string {
	var b strings.Builder
	for _, p := range t.Parts {
		if p.Kind == "text" && p.Role == "user" {
			b.WriteString(bare(p.Text))
		}
	}
	return b.String()
}

// bare is s lower-cased with everything but letters and digits taken out.
func bare(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// keep saves what was found as candidates, each from s, and counts the
// new ones. Each rests on words the user wrote (in their, as bare gives
// it): one without is the assistant's own instructions or notes said back,
// or made up, and is dropped. A kind not known is a fact; a project scope
// without a project is everyone's.
func keep(fs []sdk.MemoryDistillationProposal, s sessions.Session, proj *memory.Project, their string) int {
	n := 0
	for i, f := range fs {
		if i >= MaxPerSession {
			break
		}
		text := strings.TrimSpace(f.Text)
		if text == "" || utf8.RuneCountInString(text) > 400 {
			continue
		}
		if q := bare(f.Quote); q == "" || !strings.Contains(their, q) {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(f.Kind))
		if !slices.Contains(memory.Kinds, kind) {
			kind = memory.Fact
		}
		scope, p := memory.Global, (*memory.Project)(nil)
		if strings.EqualFold(strings.TrimSpace(f.Scope), "project") && proj != nil {
			scope, p = "project:"+proj.Key, proj
		}
		why, status := cut(strings.TrimSpace(f.Why), 300), memory.Candidate
		src := &memory.Source{Agent: s.Agent, Session: s.ID, Dir: s.Cwd, Quote: cut(strings.TrimSpace(f.Quote), 300)}
		_, added, err := memory.Owner().Save(memory.Input{Text: &text, Kind: &kind, Scope: &scope, Why: &why, Status: &status}, memory.FromInferred, src, p)
		if err == nil && added {
			n++
		}
	}
	return n
}

// Keep runs Run first after first, then every interval, until ctx ends,
// and Dream after it when one is due (Dreams); auto gives the automatic
// model at each run. Each tick applies memory's retention first
// (memory.Prune), distilling on or off.
func Keep(ctx context.Context, first, interval time.Duration, auto func() string, complete Complete) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(interval)
		// retention holds whether distilling is on or not: memories saved
		// by agents leave candidates and archived ones too
		if _, err := memory.Owner().Prune(); err != nil {
			log.Println("memory: retention:", err)
		}
		// switched off (the default): nothing to read, nor a model to
		// ask. auto picks one from every provider's models, and that
		// loaded the whole catalog (3.6 MB) for a run that read nothing
		if st, err := Load(); err == nil && !st.On {
			continue
		}
		if _, err := Run(ctx, auto(), complete); err != nil && !errors.Is(err, ErrRunning) {
			log.Println("memory: reading conversations:", err) // and in the Memory tab
		}
		if st, err := Load(); err == nil && Dreams(st, now()) {
			if _, err := Dream(ctx, auto(), complete); err != nil && !errors.Is(err, ErrRunning) {
				log.Println("memory: tidying:", err)
			}
		}
	}
}
