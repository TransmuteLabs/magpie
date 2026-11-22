package usage

import (
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

func TestViasRecountsWhenAgentIdentityChanges(t *testing.T) {
	resetUsageTest(t)
	old := PluginAgents
	t.Cleanup(func() { PluginAgents = old })
	known := []Known{
		{ID: "vias-prefix", UA: []string{"vias"}},
		{ID: "vias-before", Names: []string{"vias-exact"}},
	}
	PluginAgents = func() []Known { return known }
	set := settings.Load()
	set.ClientAgents = map[string]string{"vias-after": "claude"}
	if err := settings.Save(set); err != nil {
		t.Fatal(err)
	}

	before, after := time.Now().Add(-5*time.Minute), time.Now().Add(-time.Minute)
	const earlier = logBlockRows + 1
	for range earlier {
		Append(Record{Time: before, Agent: "Vias-Exact/1.0", Session: "thread", Provider: "relay", Model: "m", Input: 1, Status: 200})
	}
	Append(Record{Time: after, Agent: "vias-exact/2.0", Session: "thread", Provider: "relay", Model: "m", Input: 1, Status: 200})
	Append(Record{Time: after, Agent: "vias-client/2.0", Session: "thread", Provider: "relay", Model: "m", Input: 1, Status: 200})
	since := before.Add(time.Minute)

	check := func(view ViaIndex, key string, calls int) {
		t.Helper()
		got := view.Of(key)
		if len(got) != 1 || got[0].Calls != calls || got[0].Tokens != calls {
			t.Errorf("%s: %+v, want one via with %d calls and tokens", key, got, calls)
		}
	}
	first := Vias(since)
	check(Vias(time.Time{}), "vias-before|thread", earlier+1)
	check(first, "vias-before|thread", 1)
	check(first, "vias-prefix|thread", 1)
	if got, ok := GatewaySessionByID("vias-exact/1.0", "thread", nil); !ok || got.Agent != "vias-before" || got.Input != earlier+1 {
		t.Errorf("initial gateway grouping: %+v, found %t", got, ok)
	}
	deferred := Vias(since.Add(time.Second))

	known[1].ID = "vias-after"
	known[1].Names[0] = "vias-exact-renamed"
	known[1].UA = []string{"vias-exact"}
	known[0].UA[0] = "vias-client"
	current := Vias(since)
	check(current, "claude|thread", 1)
	if got := current.Of("vias-before|thread"); got != nil {
		t.Errorf("new index retained old grouping: %+v", got)
	}
	check(current, "vias-prefix|thread", 1)
	check(deferred, "vias-before|thread", 1)
	if got, ok := GatewaySessionByID("vias-exact/1.0", "thread", nil); !ok || got.Agent != "claude" || got.Input != earlier+1 {
		t.Errorf("current gateway grouping: %+v, found %t", got, ok)
	}
	if got, ok := GatewaySessionByID("vias-before", "thread", nil); ok {
		t.Errorf("old gateway grouping still listed: %+v", got)
	}

	Append(Record{Time: after, Agent: "vias-exact/3.0", Session: "thread", Provider: "relay", Model: "m", Input: 1, Status: 200})
	full := Vias(time.Time{})
	check(full, "claude|thread", earlier+2)
	if got := full.Of("vias-before|thread"); got != nil {
		t.Errorf("incremental fold retained old grouping: %+v", got)
	}
	if got, ok := GatewaySessionByID("claude", "thread", nil); !ok || got.Agent != "claude" || got.Input != earlier+2 {
		t.Errorf("incremental gateway grouping: %+v, found %t", got, ok)
	}
}
