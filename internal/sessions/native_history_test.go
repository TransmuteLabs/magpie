package sessions

import (
	"slices"
	"testing"
)

func TestNativeHistoryDelegationGuardsEveryRegisteredReader(t *testing.T) {
	for _, owner := range append(slices.Clone(nativeHistoryOwners()), "future-session-owner") {
		t.Run(owner, func(t *testing.T) {
			setup(t)
			readers := append(nativeHistoryReaders(zcodeFiles),
				nativeHistoryReader{agent: "future-session-owner", read: func() []file { return nil }})
			calls := map[string]int{}
			for index := range readers {
				reader := &readers[index]
				if reader.agent == "" {
					continue
				}
				agent := reader.agent
				reader.read = func() []file {
					calls[agent]++
					return []file{{agent: agent}}
				}
			}
			readWSL := func(agents ...string) []file {
				out := make([]file, 0, len(agents))
				for _, agent := range agents {
					calls["wsl:"+agent]++
					out = append(out, file{agent: agent})
				}
				return out
			}
			for _, view := range []*sessionSourceView{
				nil,
				{epoch: sourceEpoch.Load(), owners: map[string]bool{owner: true}},
				{epoch: sourceEpoch.Load() - 1, owners: map[string]bool{owner: true}},
			} {
				clear(calls)
				lists := nativeHistoryLists(view, readers, readWSL)
				for _, reader := range readers {
					want := 1
					if view != nil && reader.agent == owner {
						want = 0
					}
					if reader.agent != "" && calls[reader.agent] != want {
						t.Fatalf("native %s reader invoked %d times, want %d", reader.agent, calls[reader.agent], want)
					}
					for _, agent := range reader.wsl {
						want := 1
						if view != nil && agent == owner {
							want = 0
						}
						if calls["wsl:"+agent] != want {
							t.Fatalf("WSL %s reader invoked %d times, want %d", agent, calls["wsl:"+agent], want)
						}
					}
				}
				if view != nil {
					for _, files := range lists {
						if slices.ContainsFunc(files, func(item file) bool { return item.agent == owner }) {
							t.Fatal("delegated owner reached native history output")
						}
					}
				}
			}
		})
	}
}

func TestNativeHistoryReadConsentPrecedesEnumeration(t *testing.T) {
	setup(t)
	for _, owner := range nativeHistoryOwners() {
		if !nativeSourceOwner(owner) {
			t.Fatal("registered history reader escaped delegation", owner)
		}
	}
	if nativeSourceOwner("devin") || nativeSourceOwner("minimax-code") {
		t.Fatal("call-only readers acquired history ownership")
	}
	previous := OwnOff.Load()
	off := func() bool { return true }
	OwnOff.Store(&off)
	t.Cleanup(func() { OwnOff.Store(previous) })
	calls := 0
	readers := []nativeHistoryReader{
		{agent: "pi", read: func() []file { calls++; return nil }},
		{wsl: []string{"pi", "codex"}},
	}
	lists := nativeHistoryLists(nil, readers, func(...string) []file { calls++; return nil })
	if calls != 0 || len(lists) != 0 {
		t.Fatal("gateway-only history mode invoked a native reader")
	}
}
