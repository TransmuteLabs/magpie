package usage

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestKnownAgentsZeroAllocLookups(t *testing.T) {
	oldPlugin := PluginAgents
	t.Cleanup(func() { PluginAgents = oldPlugin })

	plugin := []Known{
		{ID: "plug-alpha", Name: "Plug Alpha", Names: []string{"plug-alpha", "alpha"}, UA: []string{"plug-alpha"}},
		{ID: "plug-beta", Name: "Plug Beta", Names: []string{"plug-beta"}, UA: []string{"beta-cli"}},
	}
	PluginAgents = func() []Known { return plugin }

	// Verify functional parity across Names, UA prefixes, unknown UAs, and dynamic changes.
	if got := AgentOf("plug-alpha/2.1"); got != "plug-alpha" {
		t.Fatalf("AgentOf(plug-alpha/2.1) = %q, want plug-alpha", got)
	}
	if got := AgentOf("beta-cli-next/0.1"); got != "plug-beta" {
		t.Fatalf("AgentOf(beta-cli-next/0.1) = %q, want plug-beta", got)
	}
	if got := KnownAgentID("alpha"); got != "plug-alpha" {
		t.Fatalf("KnownAgentID(alpha) = %q, want plug-alpha", got)
	}
	if got := AgentName("plug-beta"); got != "Plug Beta" {
		t.Fatalf("AgentName(plug-beta) = %q, want Plug Beta", got)
	}
	if !(Record{Agent: "magpie-router/1"}).ByMagpie() {
		t.Fatal("magpie-router/1 should count as ByMagpie")
	}

	// Lookups with non-empty PluginAgents must not allocate a merged []Known slice.
	if allocs := testing.AllocsPerRun(100, func() {
		if AgentOf("plug-alpha/2.1") != "plug-alpha" {
			t.Fatal("unexpected agent")
		}
	}); allocs > 0 {
		t.Fatalf("AgentOf allocated %.1f allocs/run, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if KnownAgentID("alpha") != "plug-alpha" {
			t.Fatal("unexpected known agent id")
		}
	}); allocs > 0 {
		t.Fatalf("KnownAgentID allocated %.1f allocs/run, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if AgentName("plug-beta") != "Plug Beta" {
			t.Fatal("unexpected agent name")
		}
	}); allocs > 0 {
		t.Fatalf("AgentName allocated %.1f allocs/run, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if !(Record{Agent: "magpie-router/1"}).ByMagpie() {
			t.Fatal("unexpected ByMagpie result")
		}
	}); allocs > 0 {
		t.Fatalf("ByMagpie allocated %.1f allocs/run, want 0", allocs)
	}

	// Dynamic plugin updates remain visible immediately.
	plugin = append(plugin, Known{ID: "plug-gamma", Name: "Plug Gamma", Names: []string{"plug-gamma"}, UA: []string{"gamma"}})
	if got := AgentOf("gamma/3.0"); got != "plug-gamma" {
		t.Fatalf("after plugin update, AgentOf(gamma/3.0) = %q, want plug-gamma", got)
	}
}

func TestSummarizeHoistsPluginAgentsOncePerPass(t *testing.T) {
	oldPlugin := PluginAgents
	t.Cleanup(func() { PluginAgents = oldPlugin })

	var calls atomic.Int64
	plugin := []Known{
		{ID: "plug-alpha", Name: "Plug Alpha", Names: []string{"plug-alpha"}, UA: []string{"plug-alpha"}},
	}
	PluginAgents = func() []Known {
		calls.Add(1)
		return plugin
	}

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	recs := make([]Record, 500)
	for index := range recs {
		agent := "plug-alpha/1.0"
		if index%5 == 0 {
			agent = "magpie-router/1"
		}
		recs[index] = Record{
			Time:            now.Add(-time.Duration(index) * time.Second),
			Agent:           agent,
			Provider:        "openai",
			ProviderKeyID:   "k1",
			ProviderAccount: "acct@example.com",
			Model:           "gpt-5",
			Input:           100,
			Output:          50,
		}
	}

	summary := summarize(Today, now, recs)
	if summary.Calls != len(recs) {
		t.Fatalf("summary.Calls = %d, want %d", summary.Calls, len(recs))
	}
	if got := calls.Load(); got > 2 {
		t.Fatalf("PluginAgents called %d times across %d rows, want <= 2", got, len(recs))
	}
	var foundModelOwn int
	for _, model := range summary.Models {
		foundModelOwn += model.Own.Calls
	}
	if foundModelOwn != 100 {
		t.Fatalf("Models Own.Calls = %d, want 100", foundModelOwn)
	}
}

func TestReadSessionAgentsCacheHitNoMergedSliceAlloc(t *testing.T) {
	oldPlugin := PluginAgents
	t.Cleanup(func() { PluginAgents = oldPlugin })

	plugin := []Known{
		{ID: "plug-alpha", Name: "Plug Alpha", Names: []string{"plug-alpha"}, UA: []string{"plug-alpha"}},
	}
	PluginAgents = func() []Known { return plugin }

	first := readSessionAgents()
	second := readSessionAgents()
	if first != second {
		t.Fatal("unchanged sessionAgents should reuse cached pointer")
	}
	if allocs := testing.AllocsPerRun(50, func() {
		if !sameKnownAgents(first.known, builtinAgents(), plugin) {
			t.Fatal("sameKnownAgents mismatch")
		}
	}); allocs > 0 {
		t.Fatalf("sameKnownAgents allocated %.1f allocs/run, want 0", allocs)
	}
	// Note: readSessionAgents calls settings.ClientAgentsNow (which may stat/read
	// settings), so we verify cache reuse and dynamic invalidation here.
	plugin = append(plugin, Known{ID: "plug-gamma", Name: "Plug Gamma", Names: []string{"plug-gamma"}, UA: []string{"gamma"}})
	third := readSessionAgents()
	if third == first {
		t.Fatal("changed PluginAgents should produce a new sessionAgents snapshot")
	}
	if got := third.reportedAs("gamma/1.0"); got != "plug-gamma" {
		t.Fatalf("third.reportedAs(gamma/1.0) = %q, want plug-gamma", got)
	}
}
