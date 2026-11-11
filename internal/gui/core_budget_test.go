package gui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What the window's core may weigh, in bytes of script without its
// whole-line comments: everything index.html loads that isn't a plugin's
// (assets/plugins/<id>/) and isn't the words (i18n.js). A page belongs in
// a plugin. Each page moved out lowers these to what is left, so a page
// can't quietly grow back into the core; a change that needs more raises
// them and says why in its commit. Comments aren't counted, so they are
// never what pays for a change (#5918); the history of each number is in
// `git log -L` of this file.
// The fork's OpenCode OAuth and ZCode reset controls add 1,107 code bytes.
// Carry that exact delta, preserving upstream's code-only headroom.
// Intrinsic ZCode adds 1,703 bytes for lazy control entrypoints, native
// sign-in routing, modal ownership and structured errors; sheets stay lazy.
const (
	appScriptBudget  = 1_087_374 // upstream 1,086,267 + the fork's 1,107
	coreScriptBudget = 1_266_143 // previous 1,264,440 + intrinsic ZCode's 1,703
)

// codeBytes is how many bytes of b are code: its blank lines, its lines
// that are only a // comment, and its /* */ blocks that start a line,
// aren't counted.
func codeBytes(b []byte) int {
	n, block := 0, false
	for _, l := range strings.SplitAfter(string(b), "\n") {
		t := strings.TrimSpace(l)
		switch {
		case block:
			block = !strings.Contains(t, "*/")
		case t == "", strings.HasPrefix(t, "//"):
		case strings.HasPrefix(t, "/*") && (strings.HasSuffix(t, "*/") || !strings.Contains(t, "*/")):
			block = !strings.HasSuffix(t, "*/")
		default:
			n += len(l)
		}
	}
	return n
}

func TestCoreScriptBudget(t *testing.T) {
	page, err := os.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	total, n := 0, 0
	for _, m := range regexp.MustCompile(`<script src="([^"?]+)`).FindAllStringSubmatch(string(page), -1) {
		name := m[1]
		if strings.HasPrefix(name, "plugins/") || name == "i18n.js" {
			continue
		}
		b, err := os.ReadFile(filepath.Join("assets", name))
		if os.IsNotExist(err) {
			continue // made by the server as it serves the page (boot.js)
		}
		if err != nil {
			t.Fatal(err)
		}
		code := codeBytes(b)
		if name == "app.js" && code > appScriptBudget {
			t.Errorf("app.js is %d bytes of code, over its budget of %d: put the page's code in its plugin, or raise the budget and say why (comments aren't counted, so cutting them doesn't help)", code, appScriptBudget)
		}
		total += code
		n++
	}
	if n < 5 {
		t.Fatalf("found only %d core scripts in index.html", n)
	}
	if total > coreScriptBudget {
		t.Errorf("the core's scripts are %d bytes of code, over their budget of %d: put the page's code in its plugin, or raise the budget and say why (comments aren't counted, so cutting them doesn't help)", total, coreScriptBudget)
	}
}
