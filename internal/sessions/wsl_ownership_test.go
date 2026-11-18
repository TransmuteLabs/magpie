package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestAstraNativeHistoryWSLSelectionPreventsDelegatedScan(t *testing.T) {
	wslDistro(t)
	previous := sourceView.Load()
	t.Cleanup(func() { sourceView.Store(previous) })
	view := &sessionSourceView{epoch: sourceEpoch.Load(), owners: map[string]bool{"pi": true}}
	sourceView.Store(view)
	readers := []nativeHistoryReader{{wsl: []string{"pi", "claude"}}}
	lists := nativeHistoryLists(view, readers, wslFiles)
	wslSettle()
	for _, list := range lists {
		for _, entry := range list {
			if entry.agent == "pi" {
				t.Fatal("control: dispatcher returned delegated Pi")
			}
		}
	}
	wslSess.Lock()
	defer wslSess.Unlock()
	for _, listing := range wslSess.lists {
		for _, entry := range listing.Files {
			if entry.Agent == "pi" {
				t.Fatalf("undelegated Claude request still enumerated delegated Pi: %s", entry.Path)
			}
		}
	}
}

func TestWSLSelectionPrecedesEveryReaderOpen(t *testing.T) {
	agents := []string{}
	for _, reader := range wslAgents {
		if !slices.Contains(agents, reader.agent) {
			agents = append(agents, reader.agent)
		}
	}
	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			home, _ := wslDistro(t)
			var mutex sync.Mutex
			var opened []string
			WSLHomes = func() []WSLHome {
				return []WSLHome{{Distro: "Ubuntu", Home: home, Running: true, Open: func(path string) string {
					mutex.Lock()
					opened = append(opened, path)
					mutex.Unlock()
					return path
				}}}
			}
			wslFiles(agent)
			wslSettle()
			want := []string{}
			for _, reader := range wslAgents {
				if reader.agent == agent {
					want = append(want, filepath.Join(home, reader.dir))
				}
			}
			mutex.Lock()
			defer mutex.Unlock()
			slices.Sort(opened)
			slices.Sort(want)
			if !slices.Equal(opened, want) {
				t.Fatalf("selected %s opened %v, want only %v", agent, opened, want)
			}
		})
	}
}

func TestWSLHistorySelectionKeepsIndependentAccounting(t *testing.T) {
	wslDistro(t)
	previous := sourceView.Load()
	t.Cleanup(func() { sourceView.Store(previous) })
	view := &sessionSourceView{epoch: sourceEpoch.Load(), owners: map[string]bool{"pi": true}}
	sourceView.Store(view)
	readers := []nativeHistoryReader{{wsl: []string{"pi", "claude"}}}
	for pass := 0; pass < 2; pass++ {
		lists := nativeHistoryLists(view, readers, wslFiles)
		wslSettle()
		var claude bool
		for _, list := range lists {
			for _, entry := range list {
				if entry.agent != "claude" {
					t.Fatalf("history selected delegated reader: %+v", entry)
				}
				claude = true
			}
		}
		if !claude {
			t.Fatalf("pass %d lost native Claude history", pass)
		}
		var pi bool
		for _, entry := range piCallFiles() {
			pi = pi || entry.agent == "pi" && entry.m().wsl == "Ubuntu"
		}
		wslSettle()
		if !pi {
			t.Fatalf("pass %d history delegation suppressed independent Pi accounting", pass)
		}
	}
}

func TestWSLSelectedRefreshPreservesOtherCachedReaders(t *testing.T) {
	home, _ := wslDistro(t)
	wslFiles("claude", "pi")
	wslSettle()
	if err := os.RemoveAll(filepath.Join(home, ".pi")); err != nil {
		t.Fatal(err)
	}
	wslSess.Lock()
	wslSess.at = time.Time{}
	wslSess.Unlock()
	wslFiles("claude")
	wslSettle()
	body, err := os.ReadFile(wslListPath())
	if err != nil {
		t.Fatal(err)
	}
	var cached map[string]*wslListing
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatal(err)
	}
	listing := cached["Ubuntu"]
	if listing == nil || !slices.ContainsFunc(listing.Files, func(entry wslEntry) bool { return entry.Agent == "pi" }) {
		t.Fatal("refreshing Claude discarded independently cached Pi history")
	}
}

func TestWSLConcurrentSelectionsListEachRequestedReader(t *testing.T) {
	home, _ := wslDistro(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); wslSettle() })
	WSLHomes = func() []WSLHome {
		return []WSLHome{{Distro: "Ubuntu", Home: home, Running: true, Open: func(path string) string {
			if path == filepath.Join(home, ".claude") {
				close(entered)
				<-release
			}
			return path
		}}}
	}
	claude, pi := make(chan []file, 1), make(chan []file, 1)
	go func() { claude <- wslFiles("claude") }()
	<-entered
	go func() { pi <- wslFiles("pi") }()
	once.Do(func() { close(release) })
	for agent, entries := range map[string][]file{"claude": <-claude, "pi": <-pi} {
		if len(entries) == 0 {
			t.Fatalf("concurrent selection lost %s", agent)
		}
		for _, entry := range entries {
			if entry.agent != agent {
				t.Fatalf("selected %s received %+v", agent, entry)
			}
		}
	}
	wslSettle()
}

func TestWSLRestoreRefreshOnlySelectsRestoredReader(t *testing.T) {
	home, _ := wslDistro(t)
	entries := wslFiles("claude")
	wslSettle()
	if len(entries) == 0 {
		t.Fatal("no Claude fixture to restore")
	}
	var mutex sync.Mutex
	var opened []string
	WSLHomes = func() []WSLHome {
		return []WSLHome{{Distro: "Ubuntu", Home: home, Running: true, Open: func(path string) string {
			mutex.Lock()
			opened = append(opened, path)
			mutex.Unlock()
			return path
		}}}
	}
	wslRelistNow([]moved{{From: entries[0].path}})
	wslSettle()
	mutex.Lock()
	defer mutex.Unlock()
	if !slices.Equal(opened, []string{filepath.Join(home, ".claude")}) {
		t.Fatalf("restoring Claude opened unrelated readers: %v", opened)
	}
}
