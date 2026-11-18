package tui

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

// tuiWords is every word the TUI hands tr or trf, read from its source:
// each literal tr and trf are called with, each label hints is (every
// second argument), and the tables whose values go through tr where they
// are shown. fmts says which of them are formats.
func tuiWords(t *testing.T) (all []string, fmts map[string]bool) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fmts = map[string]bool{}
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			all = append(all, s)
		}
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "i18n.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			lit := func(e ast.Expr) (string, bool) {
				b, ok := e.(*ast.BasicLit)
				if !ok || b.Kind != token.STRING {
					return "", false
				}
				s, err := strconv.Unquote(b.Value)
				return s, err == nil
			}
			switch fn.Name {
			case "tr", "trf":
				if len(call.Args) == 0 {
					return true
				}
				if s, ok := lit(call.Args[0]); ok {
					add(s)
					if fn.Name == "trf" {
						fmts[s] = true
					}
				}
			case "hints":
				for i := 1; i < len(call.Args); i += 2 {
					if s, ok := lit(call.Args[i]); ok {
						add(s)
					}
				}
			}
			return true
		})
	}
	for _, n := range pageNames {
		add(n)
	}
	for _, n := range periodNames {
		add(n)
	}
	for _, r := range sessRanges {
		add(r.name)
	}
	for _, api := range customAPIs {
		add(api.name)
	}
	// sessions' stats tables and chart unit, shown through tr(head) and tr(…[byCost])
	for _, s := range []string{allModels, allFolders, "models", "folders", "tokens", "cost", ruleHint, provider.TraeRefusedWords} {
		add(s)
	}
	fields, err := nativeTUIFields(agent.All(), func() ([]agent.ExtensionAgent, error) {
		return agent.ExtensionAgents(extension.Default())
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if !keepAsIs[field.Label] {
			add(field.Label)
		}
	}
	return all, fmts
}

func nativeTUIFields(all []*agent.Agent, read func() ([]agent.ExtensionAgent, error)) ([]agent.Field, error) {
	declared, err := read()
	if err != nil {
		return nil, err
	}
	owners := map[string]map[string]string{}
	for _, item := range declared {
		if _, exists := owners[item.Agent.ID]; exists {
			return nil, fmt.Errorf("duplicate declared agent %s", item.Agent.ID)
		}
		fields := map[string]string{}
		for _, field := range item.Agent.Fields {
			if _, exists := fields[field.Key]; exists {
				return nil, fmt.Errorf("duplicate declared field %s/%s", item.Agent.ID, field.Key)
			}
			fields[field.Key] = field.Label
		}
		owners[item.Agent.ID] = fields
	}
	var fields []agent.Field
	native := map[string]bool{}
	appendNative := func(row *agent.Agent) {
		if row.Plugin == "" && !native[row.ID] {
			native[row.ID] = true
			fields = append(fields, row.Fields...)
		}
	}
	for _, row := range all {
		if row.Plugin != "" {
			continue
		}
		base, _, sdkOwned := row.SDKLibraryTarget()
		if !sdkOwned {
			appendNative(row)
			continue
		}
		labels, exists := owners[row.ID]
		if !exists || len(labels) != len(row.Fields) {
			return nil, fmt.Errorf("SDK agent %s has no matching active declaration", row.ID)
		}
		for _, field := range row.Fields {
			if label, exists := labels[field.Key]; !exists || label != field.Label {
				return nil, fmt.Errorf("SDK field %s/%s has no matching declaration", row.ID, field.Key)
			}
		}
		if base != nil && base != row {
			appendNative(base)
		}
	}
	return fields, nil
}

// keepAsIs are field labels that are names, not words: the same in any
// language, so tr gives them back as they are.
var keepAsIs = map[string]bool{"magpie": true, "ultracode": true, "harness": true, "opus": true, "sonnet": true, "haiku": true, "fable": true}

var verbRe = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)

// sample is an argument for each of format's verbs, of the verb's type.
func sample(format string) []any {
	var args []any
	next := 0
	for _, m := range verbRe.FindAllStringSubmatch(format, -1) {
		v := m[0]
		if v[len(v)-1] == '%' {
			continue
		}
		i := next
		if m[1] != "" { // %[n]: the nth argument
			i, _ = strconv.Atoi(strings.Trim(m[1], "[]"))
			i--
		}
		next = i + 1
		for len(args) <= i {
			args = append(args, nil)
		}
		switch v[len(v)-1] {
		case 'd':
			args[i] = 7
		case 'f':
			args[i] = 1.5
		default:
			args[i] = "x"
		}
	}
	return args
}

// TestEveryWordHasZh fails on a word the TUI shows with no Chinese, so
// a new string can't slip in English only, and on a Chinese format whose
// verbs don't take the English one's arguments.
func TestEveryWordHasZh(t *testing.T) {
	all, fmts := tuiWords(t)
	if len(all) < 300 {
		t.Fatalf("read only %d words from the source", len(all))
	}
	for _, s := range all {
		w, ok := zh[s]
		if !ok || strings.TrimSpace(w) == "" && strings.TrimSpace(s) != "" {
			t.Errorf("no zh for %q", s)
			continue
		}
		if !fmts[s] {
			continue
		}
		args := sample(s)
		if got := fmt.Sprintf(s, args...); strings.Contains(got, "%!") {
			t.Errorf("English format %q takes its own arguments badly: %s", s, got)
		}
		if got := fmt.Sprintf(w, args...); strings.Contains(got, "%!") {
			t.Errorf("zh %q doesn't take the arguments of %q: %s", w, s, got)
		}
	}
	// a word left behind when its string went away
	used := map[string]bool{}
	for _, s := range all {
		used[s] = true
	}
	for k := range zh {
		if !used[k] {
			t.Errorf("zh has %q, which the TUI no longer shows", k)
		}
	}
}

func TestZhDateLayout(t *testing.T) {
	if got := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC).Format(zh["Jan 2"]); got != "3月4日" {
		t.Fatalf("zh's Jan 2 layout gives %q", got)
	}
}

func TestSpeak(t *testing.T) {
	for in, want := range map[string]string{"zh": "zh", "zh-TW": "zh", "en": "en", "ja": "en", "de": "en", "": "en"} {
		if got := speak(in); got != want {
			t.Errorf("speak(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPagesInChinese renders the header and a footer with Chinese picked:
// what the user sees, not just the table.
func TestPagesInChinese(t *testing.T) {
	lang = "zh"
	defer func() { lang = "en" }()
	if got := trf("%d call%s", 2, plural(2)); got != "2 次调用" {
		t.Errorf("plural in zh: %q", got)
	}
	if got := hints("q", "quit"); !strings.Contains(got, "退出") {
		t.Errorf("hints in zh: %q", got)
	}
	m := model{page: pageProviders, w: 100, h: 30}
	if got := m.header(); !strings.Contains(got, "供应商") || !strings.Contains(got, "路由") {
		t.Errorf("header in zh: %q", got)
	}
	if got := sessAgo(time.Now().Add(-3*time.Hour), time.Now()); got != "3小时前" {
		t.Errorf("sessAgo in zh: %q", got)
	}
}
