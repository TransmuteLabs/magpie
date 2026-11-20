package sessions

// Carrying a conversation over to an agent that can't read the session's
// own file (0xPlato on X): Claude Code's is converted into a new Codex
// session, and Codex's into a new Claude Code session, each in that agent's
// own folder and form, so its own resume opens it. The source file is only
// read.
//
// What goes over: what the user and the agent said, a shell command and what
// it printed as the target's own tool call and result (Claude Code's Bash,
// Codex's exec_command), every other tool's call and result as words the
// agent said (the target has no tool of that name), and the user's pasted
// images. What doesn't: reasoning. Claude Code's thinking is checked by its
// signature and Codex's by its encrypted_content, neither of which the other
// vendor can read, and a summary of it is only what the model thought. The
// target picks its model as it does for any resumed session: Codex from its
// config.toml (the rollout names none and keeps no base_instructions, so
// Codex uses its own), Claude Code from its settings; the assistant lines
// say "<synthetic>", Claude Code's own name for a line no model wrote here,
// so magpie's usage counts none of them as a call.
//
// The conversion runs when the user asks for it (CarryOver), never on a
// listing, and the one made from a file's bytes is handed back again while
// it is there, so a second click doesn't make a second session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/sessionformat"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// CarryResult is a session carried over: the new one, and the line that
// opens it in its folder.
type CarryResult struct {
	Agent   string `json:"agent"`
	ID      string `json:"id"`
	Path    string `json:"path"`
	Command string `json:"command"`
}

var linkedSessionConverters = [...]sdk.SessionConverter{
	builtincodex.ImportClaudeSession(),
	builtinclaude.ImportCodexSession(),
}

// agentThere says whether an agent is installed: its command, or its folder.
// A var so tests can say. Answers are kept a little while, as the listing
// asks it of every session on every read.
var agentThere = func(agent string) bool {
	thereMu.Lock()
	defer thereMu.Unlock()
	if t, ok := thereAt[agent]; ok && time.Since(t.at) < 30*time.Second {
		return t.ok
	}
	ok := false
	switch agent {
	case "claude":
		ok = lookOK("claude") || dirOK(ClaudeDir())
	case "codex":
		ok = lookOK("codex") || dirOK(CodexDir())
	}
	thereAt[agent] = there{ok, time.Now()}
	return ok
}

type there struct {
	ok bool
	at time.Time
}

var (
	thereMu sync.Mutex
	thereAt = map[string]there{}
)

func lookOK(name string) bool { _, err := proc.Look(name); return err == nil }
func dirOK(dir string) bool   { st, err := os.Stat(dir); return err == nil && st.IsDir() }

// carried keeps the session made from a file's bytes for a target.
var (
	carriedMu sync.Mutex
	carried   = map[carryKey]carriedSession{}
)

type carryKey struct {
	source, route, target, cwd string
	size                       int64
	mod                        time.Time
}

type carriedSession struct {
	result CarryResult
	info   fs.FileInfo
}

// CarryOver converts a session for the agent to (one carries offers it
// with Convert), and says how to open the new one.
func CarryOver(s Session, to string) (CarryResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := selectSessionHistoryOwner(ctx, s)
	if err != nil {
		return CarryResult{}, err
	}
	var selected carryOption
	eachCarry(s, func(option carryOption) bool {
		if option.Agent == to && option.Convert {
			selected = option
			return false
		}
		return true
	})
	if !selected.Convert {
		return CarryResult{}, fmt.Errorf("a %s session can't be carried over to %s", s.Agent, to)
	}
	return selected.convert(ctx, owner)
}

func convertLinkedSession(ctx context.Context, owner sessionHistoryOwner, converter sdk.SessionConverter) (CarryResult, error) {
	if owner.mode != historyNative {
		return CarryResult{}, extension.ErrActionChanged
	}
	s, to := owner.session, converter.Descriptor.To
	sourcePath := s.Path
	st, err := os.Stat(sourcePath)
	if s.Agent == "codex" && errors.Is(err, os.ErrNotExist) {
		sourcePath = rolloutTwin(sourcePath)
		st, err = os.Stat(sourcePath)
	}
	if err != nil {
		return CarryResult{}, err
	}
	store, err := resolveSessionTargetStore(converter.Descriptor)
	if err != nil {
		return CarryResult{}, err
	}
	key := carryKey{sourcePath, converter.Descriptor.ID, store.selection(), s.Cwd, st.Size(), st.ModTime()}
	carriedMu.Lock()
	defer carriedMu.Unlock()
	check := func() error { return owner.check(ctx) }
	if err := check(); err != nil {
		return CarryResult{}, err
	}
	if receipt, ok := carried[key]; ok {
		if info, err := os.Lstat(receipt.result.Path); err == nil && extension.SameSessionDocument(receipt.info, info) {
			if sourceBeforePublish != nil {
				sourceBeforePublish()
			}
			if err := check(); err != nil {
				return CarryResult{}, err
			}
			if err := store.check(ctx); err != nil {
				return CarryResult{}, err
			}
			if info, err := os.Lstat(receipt.result.Path); err == nil && extension.SameSessionDocument(receipt.info, info) {
				return receipt.result, nil
			}
			return CarryResult{}, extension.ErrActionChanged
		}
		delete(carried, key)
	}
	version := agentVersion(to)
	if err := check(); err != nil {
		return CarryResult{}, err
	}
	output, err := func() (sdk.SessionConversionOutput, error) {
		source, err := openLines(sourcePath)
		if err != nil {
			return sdk.SessionConversionOutput{}, err
		}
		defer source.Close()
		return converter.Convert(ctx, sdk.SessionConversionInput{
			Source: source, Cwd: s.Cwd, Now: time.Now(), TargetVersion: version, TargetProvider: store.provider,
		})
	}()
	if err != nil {
		return CarryResult{}, err
	}
	name, err := filepath.Localize(output.Path)
	if err != nil || !sessionInputPath(output.Path) || output.ID == "" || output.Write == nil {
		return CarryResult{}, errors.New("invalid linked session conversion output")
	}
	r := CarryResult{Agent: to, ID: output.ID, Path: filepath.Join(store.requested, name)}
	r.Command = resumeCommand("", to, r.ID, s.Cwd, r.Path)
	if r.Command == "" {
		return CarryResult{}, errors.New("linked session conversion has no resume command")
	}
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return CarryResult{}, err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(store.rootInfo, opened) {
		return CarryResult{}, errors.Join(extension.ErrActionChanged, err)
	}
	info, err := extension.PublishSessionDocument(root, path.Join(store.prefix, output.Path), func(w io.Writer) error {
		return output.Write(ctx, w)
	}, func(created map[string]fs.FileInfo, published bool) error {
		if published && sourceBeforePublish != nil {
			sourceBeforePublish()
		}
		if err := check(); err != nil {
			return err
		}
		return checkSessionTargetDuringAction(ctx, store, created)
	})
	if err != nil {
		return CarryResult{}, err
	}
	carried[key] = carriedSession{result: r, info: info}
	return r, nil
}

func eachLine(path string, fn func([]byte)) error {
	f, err := openLines(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return sessionformat.EachLine(context.Background(), f, fn)
}

// agentVersion is the version the agent wrote its newest listed session
// with, which a file written for it says too ("" when none says).
func agentVersion(agent string) string {
	for _, s := range listSessions(0) {
		if s.Agent != agent || s.Path == "" || s.WSL != "" {
			continue
		}
		v := ""
		n := 0
		eachLine(s.Path, func(b []byte) {
			if v != "" || n > 40 {
				return
			}
			n++
			var l struct {
				Version string `json:"version"`
				Payload struct {
					CLIVersion string `json:"cli_version"`
				} `json:"payload"`
			}
			if json.Unmarshal(b, &l) == nil {
				v = l.Version
				if agent == "codex" {
					v = l.Payload.CLIVersion
				}
			}
		})
		if v != "" {
			return v
		}
	}
	return ""
}
