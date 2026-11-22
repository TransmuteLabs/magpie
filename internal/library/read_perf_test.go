package library

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/yetone/magpie/internal/plugin"
)

func TestSkillTreeMemosStayBounded(t *testing.T) {
	const limit = 4096
	memo.Lock()
	originalHashes, originalTrees := memo.hashes, memo.trees
	memo.hashes = make(map[string]hashMemo)
	memo.trees = make(map[[2]string]treeMemo)
	memo.Unlock()
	t.Cleanup(func() {
		memo.Lock()
		memo.hashes, memo.trees = originalHashes, originalTrees
		memo.Unlock()
	})
	root := t.TempDir()
	original := filepath.Join(root, "original")
	write(t, filepath.Join(original, "SKILL.md"), "fixture")
	for index := range limit + 3 {
		write(t, filepath.Join(root, fmt.Sprint(index), "SKILL.md"), "fixture")
	}
	age(t, root)
	for index := range limit + 3 {
		copyDir := filepath.Join(root, fmt.Sprint(index))
		if hashFiles(copyDir, true) == "" || !sameTree(original, copyDir) {
			t.Fatalf("could not hash and compare skill %d", index)
		}
	}
	memo.Lock()
	hashes, trees := len(memo.hashes), len(memo.trees)
	memo.Unlock()
	if hashes != limit || trees != limit {
		t.Fatalf("retained %d hashes and %d comparisons for one scan of %d skill folders, want %d of each",
			hashes, trees, limit+3, limit)
	}
}

func TestLibraryReadAdoptsToolsWithoutRepeatingDiscovery(t *testing.T) {
	for _, check := range []struct {
		name string
		read func([]Problem) (*View, error)
	}{{"glance", Glance}, {"read", Read}} {
		t.Run(check.name, func(t *testing.T) {
			sandbox(t)
			fakePluginTools(t, search1api)
			inventory := pluginInventory
			var discoveries atomic.Int64
			pluginInventory = func() (plugin.List, func() map[string][]string, error) {
				discoveries.Add(1)
				return inventory()
			}
			synctest.Test(t, func(t *testing.T) {
				view, err := check.read(nil)
				if err != nil || !slices.ContainsFunc(view.Servers, func(server ServerView) bool {
					return server.Name == "search1api" && server.Local && server.Plugin == plugin.Name(search1api)
				}) {
					t.Fatalf("view did not include the adopted plugin server: %v, %v", view, err)
				}
				synctest.Wait()
				if count := discoveries.Load(); count != 1 {
					t.Fatalf("one read that adopted a server read the installed plugins %d times, want 1", count)
				}
			})
		})
	}
}

func TestLibraryReadSkipsNotesWhenNoToolsNeedOffering(t *testing.T) {
	for _, state := range []string{"empty", "disabled", "offered"} {
		t.Run(state, func(t *testing.T) {
			sandbox(t)
			plugins := fakePluginTools(t, search1api)
			switch state {
			case "empty":
				plugins.specs = nil
			case "disabled":
				plugins.off = true
			case "offered":
				ok(t)(Sync())
			}
			inventory := pluginInventory
			looked := 0
			pluginInventory = func() (plugin.List, func() map[string][]string, error) {
				list, noted, err := inventory()
				return list, func() map[string][]string {
					looked++
					return noted()
				}, err
			}
			view, err := Glance(nil)
			want := 0
			if state == "offered" {
				want = 1
			}
			if err != nil || len(view.Servers) != want {
				t.Fatalf("glance servers = %v, %v, want %d servers", view, err, want)
			}
			if looked != 0 {
				t.Fatalf("read the plugin tool notes %d times with no tool offers pending, want 0", looked)
			}
		})
	}
}

func TestLibraryToolChangesShareOneBackgroundSync(t *testing.T) {
	sandbox(t)
	names := []string{"first-tools", "second-tools", "third-tools"}
	plugins := fakePluginTools(t, names...)
	ok(t)(Sync())
	for _, name := range names {
		ok(t)(ServerAgents(name, []string{"claude"}))
	}
	synctest.Test(t, func(t *testing.T) {
		released := make(chan struct{})
		release := sync.OnceFunc(func() { close(released) })
		defer release()
		var withdrawals atomic.Int64
		was := ending
		ending = func([]*Server) {
			if withdrawals.Add(1) == 1 {
				<-released
			}
		}
		t.Cleanup(func() { ending = was })
		for _, kept := range []int{1, 2, 3} {
			plugins.specs = names[kept:]
			if kept == 2 {
				plugin.ToolsNoted()
			}
			view, err := Glance(nil)
			if err != nil || len(view.Servers) != len(names)-kept {
				t.Fatalf("glance did not publish the tool removals: %v, %v", view, err)
			}
			synctest.Wait()
		}
		if count := withdrawals.Load(); count != 1 {
			t.Errorf("%d syncs reached withdrawal while the first one was held, want 1", count)
		}
		release()
		synctest.Wait()
		if count := withdrawals.Load(); count != 2 {
			t.Errorf("%d syncs reached withdrawal after release, want 2", count)
		}
		entries, err := targetByID("claude").MCP.entries()
		if err != nil || len(entries) != 0 {
			t.Fatalf("the last removals were not applied: %v, %v", entries, err)
		}
	})
}
