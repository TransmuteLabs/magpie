package agent

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestClaudePickerMarksItsAgentWindows(t *testing.T) {
	home := syncHome(t)
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{}`)
	if err := provider.Save(provider.Provider{
		ID: "windows", Name: "Window test", Key: "k", Chat: "http://127.0.0.1:1/v1",
		Models: []string{"large", "small"}, Contexts: map[string]int{"large": 1_000_000, "small": 200_000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{
		ID: "window-pair", Members: []string{"windows/large", "windows/small"},
	}); err != nil {
		t.Fatal(err)
	}

	picker := claude(home)
	field := picker.Field("model")
	if field == nil {
		t.Fatal("Claude picker has no model field")
	}
	refs := []string{"windows/large", "windows/small", "group/window-pair"}
	for _, test := range []struct {
		name    string
		windows map[string]int
		want    []int
	}{
		{name: "catalog", want: []int{1_000_000, 200_000, 1_000_000}},
		{
			name:    "overridden",
			windows: map[string]int{"windows/large": 999_999, "windows/small": 1_000_001, "group/window-pair": 999_999},
			want:    []int{999_999, 1_000_001, 999_999},
		},
		{name: "restored", want: []int{1_000_000, 200_000, 1_000_000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := provider.SetAgentContexts("claude", test.windows); err != nil {
				t.Fatal(err)
			}
			options := picker.Picks(*field, picker.Values())
			for index, ref := range refs {
				at := slices.IndexFunc(options, func(option Option) bool { return option.Ref == ref })
				if at < 0 {
					t.Fatalf("picker omitted %q", ref)
				}
				context := test.want[index]
				value := ref
				if context >= 1_000_000 {
					value += "[1m]"
				}
				option := options[at]
				if option.Context != context || option.Value != value {
					t.Errorf("%s: picker context/value = %d/%q, want %d/%q", ref, option.Context, option.Value, context, value)
				}
			}
		})
	}
}

func TestClaudeModelPickPublishesContextWithRoute(t *testing.T) {
	home, path := desktopWindowSandbox(t)
	agent := claude(home)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var earlyContext, sawRoute, completeRoute bool
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name != target {
			return proposed
		}
		body := string(proposed)
		window := strings.Contains(body, claudeContextEnv)
		compact := strings.Contains(body, claudeCompactEnv)
		if !strings.Contains(body, `"ANTHROPIC_BASE_URL"`) {
			earlyContext = earlyContext || window || compact
		} else if !sawRoute {
			sawRoute = true
			completeRoute = window && compact
		}
		return proposed
	}, func() error { return agent.Apply("model", "v/gpt") })
	if err != nil || earlyContext || !completeRoute {
		t.Fatalf("Apply = %v, context published before route = %t, route contained both windows = %t", err, earlyContext, completeRoute)
	}
	if got, _ := edit.GetJSON(path, "env."+claudeContextEnv); got != "272000" {
		t.Fatalf("context window = %q", got)
	}
	if err := edit.SetJSON(path, edit.KV{Path: "env." + claudeCompactEnv, Value: "222000"}); err != nil {
		t.Fatal(err)
	}
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, present := edit.GetJSON(path, "env."+claudeContextEnv); present {
		t.Error("Disconnect kept its own context window")
	}
	if got, _ := edit.GetJSON(path, "env."+claudeCompactEnv); got != "222000" {
		t.Errorf("Disconnect changed the user's compaction window to %q", got)
	}
}

func TestClaudeSyncPreservesConcurrentContextEdit(t *testing.T) {
	home, path := desktopWindowSandbox(t)
	agent := claude(home)
	if err := agent.Apply("model", "v/gpt"); err != nil {
		t.Fatal(err)
	}
	before := stashLoad()
	vendor, err := provider.Find("v")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetContext(*vendor, "gpt", 355000); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetCompactAt(210000); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var external string
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name == target && external == "" && strings.Contains(string(proposed), "355000") {
			body, err := edit.PatchJSON([]byte(readFile(name)), []edit.KV{
				{Path: "env." + claudeContextEnv, Value: "233000"},
				{Path: "env." + claudeCompactEnv, Value: "144000"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			external = string(body)
			writeFile(t, name, external)
		}
		return proposed
	}, agent.Sync)
	if external == "" || !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
		t.Errorf("Sync = %v, concurrent settings preserved = %t", err, external != "" && readFile(path) == external)
	}
	after := stashLoad()
	for _, key := range []string{"claude.context_tokens", "claude.compact_window", "claude.compact_long"} {
		if after[key] != before[key] {
			t.Errorf("rejected Sync changed %s from %q to %q", key, before[key], after[key])
		}
	}
	if t.Failed() {
		return
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{claudeContextEnv: "233000", claudeCompactEnv: "144000"} {
		if got, _ := edit.GetJSON(path, "env."+name); got != want {
			t.Errorf("retry changed %s to %q, want %q", name, got, want)
		}
	}
}

// TestClaudeContextWindow: Claude Code takes a model it doesn't know for a
// 200K one, so magpie tells it the window of the model it switches it to
// (CLAUDE_CODE_MAX_CONTEXT_TOKENS), changes it with the model, takes it away
// for a model it doesn't know the window of and for Claude's own, and never
// touches a value the user set (#311).
func TestClaudeContextWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := provider.Save(provider.Provider{ID: "v", Name: "V", Chat: "https://example.test/v1", Key: "k",
		Models: []string{"small", "mid", "big", "plain"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("v", "https://example.test/v1", []catalog.Model{
		{ID: "small", Context: 128000}, {ID: "mid", Context: 400000}, {ID: "big", Context: 1000000}, {ID: "plain"},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{"theme":"dark"}`)
	a := claude(home)
	window := func() string { v, _ := edit.GetJSON(path, "env."+claudeContextEnv); return v }
	set := func(key, v string) {
		t.Helper()
		if err := a.Field(key).Set(v); err != nil {
			t.Fatal(err)
		}
	}

	set("model", "v/small")
	if window() != "128000" {
		t.Fatalf("small: %q", window())
	}
	set("model", "v/mid")
	if window() != "400000" {
		t.Fatalf("switched to mid: %q", window())
	}
	set("model", "v/plain")
	if window() != "" {
		t.Fatalf("a model of no known window keeps the last one's: %q", window())
	}
	// a 1M model is marked [1m], which Claude Code takes for 1M whatever
	// the window says; beside a tier that isn't, the window told is the
	// main's own, and the tier compacts by its own (多喝热水 on Discord:
	// the tier's read as the main's), under Full window and without it
	set("model", "v/big[1m]")
	if window() != "" {
		t.Fatalf("[1m]: %q", window())
	}
	budget := func(id string) string { v, _ := edit.GetJSON(path, budgetPath(id)); return v }
	compact := func() (env, top string) {
		env, _ = edit.GetJSON(path, "env."+claudeCompactEnv)
		top, _ = edit.GetJSON(path, "autoCompactWindow")
		return env, top
	}
	set("haiku", "v/small")
	if e, top := compact(); window() != "1000000" || budget("v/small") != "128000" || budget("v/big[1m]") != "" || e != "" || top != "272000" {
		t.Fatalf("haiku on small beside a [1m] model: window %q, small %q, big %q, compact env %q, top-level %q",
			window(), budget("v/small"), budget("v/big[1m]"), e, top)
	}
	if err := settings.Save(settings.Settings{FullContext: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if e, top := compact(); window() != "1000000" || budget("v/small") != "128000" || e != "" || top != "" {
		t.Fatalf("full window: window %q, small %q, compact env %q, top-level %q", window(), budget("v/small"), e, top)
	}
	if err := settings.Save(settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	set("haiku", "")
	if e, top := compact(); window() != "" || budget("v/small") != "" || e != "272000" || top != "" {
		t.Fatalf("the tier gone: window %q, small %q, compact env %q, top-level %q", window(), budget("v/small"), e, top)
	}
	set("model", "v/mid")
	if window() != "400000" {
		t.Fatalf("the main model's window wins: %q", window())
	}
	// back to Claude's own model, and to Claude Code as installed
	set("model", "claude-opus-4-8")
	if window() != "" {
		t.Fatalf("Claude's own model kept magpie's window: %q", window())
	}
	set("model", "v/mid")
	set("model", "")
	if window() != "" {
		t.Fatalf("reset kept magpie's window: %q", window())
	}
	if v, _ := edit.GetJSON(path, "theme"); v != "dark" {
		t.Fatal("theme lost")
	}

	// the user's own value is theirs, set before magpie or over magpie's
	if err := edit.SetJSON(path, edit.KV{Path: "env." + claudeContextEnv, Value: "64000"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v/small", "v/plain", "v/mid", "claude-opus-4-8", ""} {
		set("model", v)
		if window() != "64000" {
			t.Fatalf("the user's value after %q: %q", v, window())
		}
	}
	if err := edit.DelJSON(path, "env."+claudeContextEnv); err != nil {
		t.Fatal(err)
	}
	set("model", "v/small")
	if err := edit.SetJSON(path, edit.KV{Path: "env." + claudeContextEnv, Value: "100000"}); err != nil {
		t.Fatal(err)
	}
	set("model", "v/mid")
	set("model", "")
	if window() != "100000" {
		t.Fatalf("a value the user changed magpie's to: %q", window())
	}
	// and one the same as magpie would write, but not written by it
	set("model", "v/small")
	if window() != "100000" {
		t.Fatalf("user value replaced: %q", window())
	}
	if err := edit.SetJSON(path, edit.KV{Path: "env." + claudeContextEnv, Value: "400000"}); err != nil {
		t.Fatal(err)
	}
	set("model", "")
	if window() != "400000" {
		t.Fatalf("the user's 400000 was taken for magpie's: %q", window())
	}
}
