package sessions

// A session's whole conversation, read from the agent's own file when it is
// asked for: what was said, in order, by the user, the model and its tools.
// The file is only read; magpie keeps no copy.

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"

	"github.com/tidwall/gjson"
)

// Transcript is what was said in a session, in the order it was said.
type Transcript struct {
	Parts []Part `json:"parts"`
	Cut   bool   `json:"cut,omitempty"` // there was more than is kept here
	// From is the parts before the first one kept, left out so that the
	// part a search found is shown (TranscriptAt)
	From     int    `json:"from,omitempty"`
	Source   string `json:"source,omitempty"`
	Captured int    `json:"captured,omitempty"`
}

// transcriptMax is the characters of all of a transcript's parts.
const transcriptMax = 400_000

// ErrNoTranscript is a session whose conversation magpie can't read: an
// agent that keeps it in a database, or in a form not read here.
var ErrNoTranscript = errors.New("this agent's conversations can't be shown")

// HasTranscript says whether an agent's sessions can be read as a
// transcript.
func HasTranscript(agent string) bool {
	if extensionSourceAgent(agent) {
		view := sourceView.Load()
		for _, record := range view.records {
			if record.grant.descriptor.Agent == agent && record.grant.descriptor.Transcript {
				return true
			}
		}
		return false
	}
	switch agent {
	case "claude", "qoder", "qoder-cn", "codex", "pi", "raft", "cindy", "omp", "cursor", "opencode", "kimi", "agy", "antigravity", "openmausbot", "liveagent", "workbuddy", "workbuddy-ai", "codebuddy":
		return true
	}
	return false
}

// readable says whether a session's conversation can be read: its agent's
// can, and its file is the one they are read from (an agy conversation
// listed from its row alone has none).
func readable(s Session) bool {
	return HasTranscript(s.Agent) && !(s.Agent == "agy" && agyRowOnly(s.Path))
}

func (t *Transcript) add(_ bool, p Part) bool {
	size := 0
	for _, x := range t.Parts {
		size += len(x.Text)
	}
	if size >= transcriptMax {
		t.Cut = true
		return false
	}
	if p, ok := keep(p); ok {
		t.Parts = append(t.Parts, p)
	}
	return true
}

// TranscriptOf reads a session's conversation from its (main) file.
func TranscriptOf(s Session) (Transcript, error) {
	return TranscriptOfContext(context.Background(), s)
}

func TranscriptOfContext(ctx context.Context, s Session) (out Transcript, err error) {
	owner, err := selectSessionHistoryOwner(ctx, s)
	if err != nil {
		if errors.Is(err, ErrNotRead) {
			return Transcript{Parts: []Part{}}, err
		}
		return Transcript{}, err
	}
	defer func() {
		if changed := owner.check(ctx); changed != nil {
			out, err = Transcript{}, changed
		}
	}()
	if owner.mode == historyDelegated {
		return extensionSourceTranscript(ctx, owner.session.source)
	}
	out = Transcript{Parts: []Part{}}
	if !HasTranscript(s.Agent) || s.Path == "" {
		return out, ErrNoTranscript
	}
	err = owner.walkParts(ctx, historyRead{}, out.add)
	return out, err
}

// transcriptLead is the parts kept before the one TranscriptAt finds,
// when the ones before it are left out.
const transcriptLead = 6

// TranscriptAt is TranscriptOf for a reader brought to the part that
// holds needle (NeedleOf), the first said one, else the first anywhere,
// as SearchSession's hit: when TranscriptOf would be cut before that
// part, the parts before it are left out (but a few) and From says how
// many were.
func TranscriptAt(s Session, needle string) (Transcript, error) {
	return TranscriptAtContext(context.Background(), s, needle)
}

func TranscriptAtContext(ctx context.Context, session Session, needle string) (out Transcript, err error) {
	if needle == "" {
		return TranscriptOfContext(ctx, session)
	}
	owner, err := selectSessionHistoryOwner(ctx, session)
	if err != nil {
		if errors.Is(err, ErrNotRead) {
			return Transcript{Parts: []Part{}}, err
		}
		return Transcript{}, err
	}
	defer func() {
		if changed := owner.check(ctx); changed != nil {
			out, err = Transcript{}, changed
		}
	}()
	if owner.mode == historyDelegated {
		return extensionSourceTranscript(ctx, owner.session.source)
	}
	if !HasTranscript(session.Agent) || session.Path == "" {
		return Transcript{Parts: []Part{}}, ErrNoTranscript
	}
	// the parts TranscriptOf keeps are counted as it keeps them, and the
	// found one's place among them noted, and the characters before it
	at, saidAt, n, size, sizeAt, saidSize := -1, -1, 0, 0, 0, 0
	if err := owner.walkParts(ctx, historyRead{}, func(_ bool, p Part) bool {
		p, ok := keep(p)
		if !ok {
			return true
		}
		if searched(p) {
			if _, hit := hitIn(p, needle); hit {
				if at < 0 {
					at, sizeAt = n, size
				}
				if said(p) && saidAt < 0 {
					saidAt, saidSize = n, size
				}
			}
		}
		n++
		size += len(p.Text)
		return saidAt < 0
	}); err != nil {
		return Transcript{Parts: []Part{}}, err
	}
	if err := owner.check(ctx); err != nil {
		return Transcript{Parts: []Part{}}, err
	}
	if saidAt >= 0 {
		at, sizeAt = saidAt, saidSize
	}
	if at < 0 || sizeAt < transcriptMax {
		out = Transcript{Parts: []Part{}}
		err = owner.walkParts(ctx, historyRead{}, out.add)
		return out, err
	}
	out = Transcript{Parts: []Part{}, From: max(0, at-transcriptLead)}
	i := 0
	err = owner.walkParts(ctx, historyRead{}, func(in bool, p Part) bool {
		if _, ok := keep(p); !ok {
			return true
		}
		i++
		if i <= out.From {
			return true
		}
		return out.add(in, p)
	})
	return out, err
}

func readHistoryTranscript(ctx context.Context, owner sessionHistoryOwner, keep func([]byte) bool, add func(bool, Part) bool) error {
	return readHistoryParts(ctx, owner, keep, false, add)
}

func readHistoryParts(ctx context.Context, owner sessionHistoryOwner, keep func([]byte) bool, spokenOnly bool, add func(bool, Part) bool) error {
	return owner.readParts(ctx, historyRead{keep: keep, spokenOnly: spokenOnly}, add)
}

func readDelegatedHistoryParts(ctx context.Context, session Session, read historyRead, add func(bool, Part) bool) error {
	transcript, err := extensionSourceTranscript(ctx, session.source)
	if err != nil {
		return err
	}
	for _, part := range transcript.Parts {
		if read.spokenOnly && !said(part) {
			continue
		}
		if !add(part.Role == "user", part) {
			break
		}
	}
	return nil
}

func readCodexHistoryParts(ctx context.Context, session Session, read historyRead, add func(bool, Part) bool) error {
	return codexContentOf(ctx, read.call(session), read.keep, read.spokenOnly, add)
}

func readClaudeHistoryParts(ctx context.Context, session Session, read historyRead, add func(bool, Part) bool) error {
	if session.BranchOf != "" && read.keep == nil && read.span == nil {
		return branchTranscript(ctx, session, read.spokenOnly, add)
	}
	return claudeContentOf(ctx, read.call(session), read.keep, read.spokenOnly, add)
}

func readNativeHistoryParts(ctx context.Context, s Session, read historyRead, add func(bool, Part) bool) error {
	keep, spokenOnly := read.keep, read.spokenOnly
	switch s.Agent {
	case "opencode":
		return openCodeTranscript(ctx, s, spokenOnly, add)
	case "cursor":
		return cursorTranscript(ctx, s.Path, spokenOnly, add)
	case "pi", "raft", "cindy", "omp":
		return piTranscript(ctx, s.Path, spokenOnly, add)
	case "kimi":
		return kimiTranscript(ctx, s.Path, keep, spokenOnly, add)
	case "agy":
		return agyTranscript(ctx, s.Path, keep, spokenOnly, add)
	case "antigravity":
		return antigravityTranscriptOf(ctx, s.Path, keep, spokenOnly, add)
	case "openmausbot":
		return ombTranscript(ctx, s, spokenOnly, add)
	case "liveagent":
		return liveagentTranscript(ctx, s.Path, spokenOnly, add)
	case "workbuddy", "workbuddy-ai", "codebuddy":
		return wbTranscript(ctx, s.Path, spokenOnly, add)
	default:
		return readClaudeHistoryParts(ctx, s, read, add)
	}
}

// readTranscript tells add each part of a session's conversation, in order,
// as its agent's file has it.
func readTranscript(s Session, add func(bool, Part) bool) error {
	return readTranscriptOf(s, nil, add)
}

// readTranscriptOf is readTranscript decoding only the lines keep says yes
// to, where a line stands alone (Claude's, Codex's, Kimi's); Pi's lines are a tree,
// read whole.
func readTranscriptOf(s Session, keep func([]byte) bool, add func(bool, Part) bool) error {
	ctx := context.Background()
	owner, err := selectSessionHistoryOwner(ctx, s)
	if err != nil {
		return err
	}
	return readHistoryTranscript(ctx, owner, keep, add)
}

// branchTranscript reads a branch (ccFamily) as its conversation went: the
// session it is a branch of from its start up to the last message the two
// share, then what the branch said on its own. A fork made after a
// compaction holds only the summary and what follows it, and opened on
// that summary and one turn (Neo, #feedback). A search reads the branch's
// own file only, so a hit in the shared part is the head's, not each
// branch's again.
func branchTranscript(ctx context.Context, s Session, spokenOnly bool, add func(bool, Part) bool) error {
	ends := map[string]int64{}
	if _, err := scanThinContext(ctx, s.BranchOf, 0, nil, thinClaude, func(b []byte, _, end int64) bool {
		if u := ccMessageUUID(b); u != "" {
			ends[u] = end
		}
		return true
	}); err != nil {
		// the head unread: the branch alone, as before it was known to be one
		return claudeContentOf(ctx, Call{Agent: s.Agent, File: s.Path, To: math.MaxInt64}, nil, spokenOnly, add)
	}
	fork := int64(-1)
	if _, err := scanThinContext(ctx, s.Path, 0, nil, thinClaude, func(b []byte, _, _ int64) bool {
		if e, ok := ends[ccMessageUUID(b)]; ok && e > fork {
			fork = e
		}
		return true
	}); err != nil {
		return err
	}
	if fork < 0 {
		return claudeContentOf(ctx, Call{Agent: s.Agent, File: s.Path, To: math.MaxInt64}, nil, spokenOnly, add)
	}
	more := true
	if err := claudeContentOf(ctx, Call{Agent: s.Agent, File: s.BranchOf, To: fork}, nil, spokenOnly, func(in bool, p Part) bool {
		more = add(in, p) && more
		return more
	}); err != nil || !more {
		return err
	}
	return claudeContentOf(ctx, Call{Agent: s.Agent, File: s.Path, To: math.MaxInt64}, func(b []byte) bool {
		_, shared := ends[ccMessageUUID(b)]
		return !shared
	}, spokenOnly, add)
}

// ccMessageUUID is the uuid of a Claude-format user or assistant line, ""
// for any other line.
func ccMessageUUID(b []byte) string {
	if !bytes.Contains(b, ccUser) && !bytes.Contains(b, ccAssistant) {
		return ""
	}
	r := gjson.GetManyBytes(b, "type", "uuid")
	if t := r[0].String(); (t == "user" || t == "assistant") && r[1].Type == gjson.String {
		return r[1].String()
	}
	return ""
}

type piPosition struct {
	parent     string
	start, end int64
	parts      func() []Part
}

// piTranscript reads the branch the session is on: a Pi session is a tree,
// each entry naming its parent, and the last entry written is where it
// stands (an earlier branch, left by /tree or a retry, isn't said again).
func piTranscript(ctx context.Context, path string, spokenOnly bool, add func(bool, Part) bool) error {
	if scanHook != nil {
		scanHook(path, 0)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := openLines(path)
	if err != nil {
		return err
	}
	defer source.Close()
	plain, seekable := source.(*os.File)
	codec := piTranscriptCodec
	byID := map[string]piPosition{}
	last := ""
	_, err = scanLines(ctx, source, 0, nil, nil, func(b []byte, start, end int64) bool {
		node, ok := codec.Inspect(b, spokenOnly)
		if !ok {
			return true
		}
		position := piPosition{parent: node.ParentID}
		if node.HasContent {
			position.start, position.end = start, end
			if !seekable {
				record, err := codec.Decode(b, spokenOnly)
				if err != nil {
					return true
				}
				position.parts = record.Parts
			}
		}
		byID[node.ID], last = position, node.ID
		return true
	})
	if err != nil {
		return err
	}
	var branch []string
	for id, n := last, 0; id != "" && n <= len(byID); n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, ok := byID[id]
		if !ok {
			break
		}
		if e.end != e.start {
			branch = append(branch, id)
		}
		id = e.parent
	}
	var buf []byte
	for i := len(branch) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := branch[i]
		position := byID[id]
		parts := position.parts
		if seekable {
			size := int(position.end - position.start)
			if cap(buf) < size {
				buf = make([]byte, size)
			}
			buf = buf[:size]
			if _, err := plain.ReadAt(buf, position.start); err != nil {
				return err
			}
			record, err := codec.Decode(buf, spokenOnly)
			if err != nil {
				return err
			}
			if record.ID != id || record.ParentID != position.parent {
				return errors.New("the Pi session tree changed while reading")
			}
			parts = record.Parts
		}
		for _, p := range parts() {
			if !add(p.Role != "assistant", p) {
				return nil
			}
		}
	}
	return nil
}
