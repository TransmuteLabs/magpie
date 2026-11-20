package sessions

// Carrying a session on in another agent (#845): only where that agent reads
// the session's own file as it is, so nothing is converted, and the file is
// left as it was.
//
// oh-my-pi grew from Pi and reads Pi's sessions: `omp --fork <file>` loads a
// Pi session (its version-3 entries, messages, tool calls and results,
// thinking; the model from its assistant messages, as omp names models
// provider/id) and writes a new omp session from it, the Pi file untouched.
// The other way doesn't work: Pi opens a file only when its first line is
// the session header, and omp's first line is its title. Claude Code's and
// Codex's sessions are converted for each other (carry_convert.go).

import (
	"context"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Carry is another agent a session can be carried on in, and the shell
// line that does it in the session's folder. A Convert one has no line
// yet: the session is converted into a new one of that agent's when the
// user asks (CarryOver, carry_convert.go), and the line opens that.
type Carry struct {
	Agent   string `json:"agent"`
	Command string `json:"command,omitempty"`
	Convert bool   `json:"convert,omitempty"`
}

// ompThere says whether oh-my-pi is installed: its command, or its folder.
// A var so tests can say.
var ompThere = func() bool {
	if _, err := proc.Look("omp"); err == nil {
		return true
	}
	_, err := os.Stat(OmpDir())
	return err == nil
}

// carries are the other agents a session can be carried on in.
func carries(s Session) (out []Carry) {
	eachCarry(s, func(option carryOption) bool {
		out = append(out, option.Carry)
		return true
	})
	return out
}

type carryOption struct {
	Carry
	linked    *sdk.SessionConverter
	installed SessionExtension
}

func (option carryOption) convert(ctx context.Context, owner sessionHistoryOwner) (CarryResult, error) {
	if option.linked != nil {
		return convertLinkedSession(ctx, owner, *option.linked)
	}
	return carryExtensionOwned(ctx, owner, option.installed)
}

func eachCarry(s Session, offer func(carryOption) bool) {
	native := s.source == nil
	if !native && (s.source.stale || s.source.storage != nil) {
		return
	}
	if s.ReadOnly || s.WSL != "" || s.Path == "" || s.Cwd == "" {
		return
	}
	directOMP := native && s.Agent == "pi" && ompThere()
	if directOMP {
		if !offer(carryOption{Carry: Carry{Agent: "omp", Command: inFolder(s.Cwd, "omp --fork "+argQuote(s.Path))}}) {
			return
		}
	}
	routes := extensionSessionRoutes(s.Agent)
	if native {
		for index := range linkedSessionConverters {
			converter := &linkedSessionConverters[index]
			if converter.Descriptor.From != s.Agent {
				continue
			}
			target := converter.Descriptor.To
			choice := carryOption{Carry: Carry{Agent: target, Convert: true}}
			installed := slices.IndexFunc(routes, func(route SessionExtension) bool { return route.Session.To == target })
			if installed >= 0 {
				choice.installed = routes[installed]
				routes = slices.Delete(routes, installed, installed+1)
			} else if agentThere(target) {
				choice.linked = converter
			} else {
				continue
			}
			if !offer(choice) {
				return
			}
		}
	}
	for _, route := range routes {
		if directOMP && route.Session.To == "omp" {
			continue
		}
		if !offer(carryOption{Carry: Carry{Agent: route.Session.To, Convert: true}, installed: route}) {
			return
		}
	}
}

// inFolder is a shell line running run in the folder cwd.
func inFolder(cwd, run string) string {
	if runtime.GOOS == "windows" {
		return "Set-Location -LiteralPath " + argQuote(cwd) + "; " + run
	}
	return "cd " + argQuote(cwd) + " && " + run
}

// argQuote is s as one argument of this system's shell.
func argQuote(s string) string {
	if runtime.GOOS == "windows" {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return shellQuote(s)
}
