package tui

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
)

func TestGroupRoutingCycleUsesCatalog(t *testing.T) {
	home(t)
	const strategy = "test-plugin/reverse"
	remove := pluginrt.ProcessRuntime().RoutingCatalog().Register(routes.Option{
		ID: strategy, Name: "Reverse", Plugin: "test-plugin",
	}, routes.Ordered)
	t.Cleanup(remove)
	if err := provider.SaveGroup(provider.Group{
		ID: "choice", Name: "Choice", Routing: provider.Manual,
		Members: []string{"a/claude-opus-5-5", "b/gpt-5.5"},
	}); err != nil {
		t.Fatal(err)
	}
	press(t, openGroup(t, "choice", 40), "o")
	if got := group(t, "choice").Routing; got != strategy {
		t.Fatalf("routing after Manual is %q, want %q", got, strategy)
	}
	remove()
	opened := openGroup(t, "choice", 40)
	if !strings.Contains(opened.View(), strategy) {
		t.Fatal("stored routing disappeared from the group after removal")
	}
	press(t, opened, "o")
	if got := group(t, "choice").Routing; got != provider.Ordered {
		t.Fatalf("routing after an unavailable strategy is %q, want %q", got, provider.Ordered)
	}
}
