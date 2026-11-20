package sessions

// What was said in a call — what the agent was given and what came back —
// read from the session file the call is in when it is asked for, from the place
// the call has in it (Call.File, From, To); magpie keeps no copy.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/extension/builtin/claude"
	"github.com/yetone/magpie/internal/extension/builtin/codex"
	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Part is one thing said in a call.
type Part = sdk.SessionSourcePart

// Content is what a call was given and what it said, in the order of the file.
type Content struct {
	Input  []Part `json:"input"`
	Output []Part `json:"output"`
	Cut    bool   `json:"cut,omitempty"` // there was more than is kept here
}

const (
	partMax    = 8000    // characters of one part
	contentMax = 160_000 // of all of them
)

var errNoPlace = errors.New("the call has no place in a file")

// add keeps a part, cut to what one may be, unless the content is full; it
// says whether there is room for more.
func (c *Content) add(input bool, p Part) bool {
	size := 0
	for _, x := range c.Input {
		size += len(x.Text)
	}
	for _, x := range c.Output {
		size += len(x.Text)
	}
	if size >= contentMax {
		c.Cut = true
		return false
	}
	p, ok := keep(p)
	if !ok {
		return true
	}
	if input {
		c.Input = append(c.Input, p)
	} else {
		c.Output = append(c.Output, p)
	}
	return true
}

// keep is a part trimmed and cut to what one may be, unless it says nothing.
func keep(p Part) (Part, bool) {
	p.Text = strings.TrimSpace(p.Text)
	if p.Text == "" {
		return p, false
	}
	if n := utf8.RuneCountInString(p.Text); n > partMax {
		r := []rune(p.Text)
		p.Text, p.Cut = string(r[:partMax]), p.Cut+n-partMax
	}
	return p, true
}

// ContentOf reads what was said in a call from its file.
func ContentOf(c Call) (Content, error) {
	return LocatedCall{Call: c, source: c.Agent, agent: c.Agent}.Content()
}

type LocatedCall struct {
	Call
	source, agent string
}

func (located LocatedCall) Content() (Content, error) {
	c := located.Call
	ctx := context.Background()
	out := Content{Input: []Part{}, Output: []Part{}}
	if located.source != located.agent {
		if err := checkNativeHistoryOwner(ctx, located.agent); err != nil {
			return out, err
		}
	}
	owner, err := selectSessionHistoryOwner(ctx, Session{Agent: located.source, Path: c.File})
	if err != nil {
		return out, err
	}
	if c.File == "" || c.Agent == "opencode" { // OpenCode's are rows, not lines of a file
		return out, errNoPlace
	}
	err = owner.readParts(ctx, historyRead{span: &historySpan{from: c.From, to: c.To, message: c.Msg}}, out.add)
	if located.source != located.agent {
		err = errors.Join(err, checkNativeHistoryOwner(ctx, located.agent))
	}
	if err != nil {
		return Content{Input: []Part{}, Output: []Part{}}, err
	}
	return out, nil
}

// FindCall is the call of a session made between two times that ended
// nearest at (a call's Time is its last line's), if the session files have
// one. A gateway request's window can hold the next calls of a quick tool
// loop too; the newest of them would be another request's.
func FindCall(agent, session string, from, to, at time.Time) (LocatedCall, bool) {
	ctx := context.Background()
	if session == "" || checkNativeHistoryOwner(ctx, agent) != nil {
		return LocatedCall{}, false
	}
	var files []file
	switch agent {
	case "claude":
		files = distinctOf(claudeFiles(), wslFiles("claude"))
	case "claude-desktop":
		var code []file
		var wsl []file
		if checkNativeHistoryOwner(ctx, "claude") == nil {
			code, wsl = claudeFiles(), wslFiles("claude")
		}
		files = distinctOf(claudeDesktopFiles(), code, wsl)
	case "codex":
		files = distinctOf(codexFiles(), wslFiles("codex"))
	}
	var best, desktopFallback LocatedCall
	dist := func(t time.Time) time.Duration { return max(t.Sub(at), at.Sub(t)) }
	closer := func(candidate Call, current LocatedCall) bool {
		return current.Agent == "" ||
			cmp.Or(cmp.Compare(dist(current.Time), dist(candidate.Time)),
				candidate.Time.Compare(current.Time), cmp.Compare(candidate.File, current.File),
				cmp.Compare(candidate.To, current.To)) > 0
	}
	visitCalls(allowed(files), from.Add(-time.Minute), func(source string, c Call) {
		if c.Session != session || c.Time.Before(from) || c.Time.After(to) {
			return
		}
		switch {
		case c.Agent == agent:
			if closer(c, best) {
				best = LocatedCall{Call: c, source: source, agent: agent}
			}
		case agent == "claude-desktop" && c.Agent == "claude":
			if closer(c, desktopFallback) {
				desktopFallback = LocatedCall{Call: c, source: source, agent: agent}
			}
		}
	})
	if best.Agent == "" {
		best = desktopFallback
	}
	if best.Agent == "" || checkNativeHistoryOwner(ctx, best.source) != nil || checkNativeHistoryOwner(ctx, agent) != nil {
		return LocatedCall{}, false
	}
	return best, true
}

// claudeContentOf reads the lines in a call or transcript, optionally
// applying a search's cheap test of a line before decoding it.
func claudeContentOf(ctx context.Context, c Call, keep func([]byte) bool, spokenOnly bool, add func(input bool, p Part) bool) error {
	_, err := scanThinContext(ctx, c.File, c.From, nil, nil, func(b []byte, start, end int64) bool {
		if start >= c.To {
			return false
		}
		if keep != nil && !keep(b) {
			return true
		}
		return claude.TranscriptRecord(b, c.Msg, spokenOnly, add)
	})
	return err
}

func textPart(role, text string) Part { return sessionformat.TextPart(role, text) }

func resultText(raw json.RawMessage) string { return sessionformat.ResultText(raw) }

func pretty(raw json.RawMessage) string { return sessionformat.Pretty(raw) }

// codexContentOf reads the items between the call before and this one: what the
// model was given (the prompt, a tool's output) and what it said (words,
// reasoning, a call of a tool). A rollout the Codex app has compressed (or
// unpacked) since the call was read is read in its new form: its lines and
// their places are the same. add is as claudeContent's.
func codexContentOf(ctx context.Context, c Call, keep func([]byte) bool, spokenOnly bool, add func(input bool, p Part) bool) error {
	path := c.File
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(rolloutTwin(path)); err == nil {
			path = rolloutTwin(path)
		}
	}
	var decoder codex.TranscriptDecoder
	_, err := scanThinContext(ctx, path, c.From, nil, nil, func(b []byte, start, end int64) bool {
		if start >= c.To {
			return false
		}
		return decoder.Record(b, keep, spokenOnly, add)
	})
	return err
}
