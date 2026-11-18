package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sessionCacheSnapshotFixture() *state {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	checkpoint := &tail{Off: 1, Size: 2}
	return &state{
		Agent: "claude", Cwd: "/original", Title: "original", Size: 2, Start: at, Last: at,
		Codex:         &codexUsageState{Models: map[string]string{"transient": "model"}},
		CodexIdentity: &CodexIdentity{ID: "session", Role: "agent"},
		Total:         &Tokens{Input: 2},
		Models:        named[Tokens]{{"model", Tokens{Input: 2}}},
		Days: named[*day]{{"2026-11-01", &day{Models: named[Tokens]{{"model", Tokens{Input: 2}}},
			Hours: hours{1}, Tools: named[int]{{"tool", 1}}, Skills: named[int]{{"skill", 1}}}}},
		Claude: claudeWindowOf([]claudeWindowEntry{{Key: "message", M: &claudeContribution{
			Request: "request", Updated: at, Usage: &claudeUsageVersion{At: at, Tokens: Tokens{Input: 2}},
			Previous: []claudeUsageVersion{{At: at, Tokens: Tokens{Input: 1}}}, Blocks: map[string]bool{"block": true},
			Tools: map[string]claudeToolUsage{"tool": {Name: "tool", Previous: []claudeToolVersion{{Name: "old-tool"}}}},
		}}}),
		StateMore: &StateMore{
			Named: "original", Compacts: []string{"compact"}, Convs: []string{"conversation"},
			ResponseIDs: []string{"response"}, SeedConvs: []string{"seed"}, TurnConvs: []string{"turn"},
			CodexSpawnNames: map[string]string{"child": "name"}, CodexSpawnConflicts: map[string]bool{"child": true},
			CodexAgentIDs: map[string]string{"child": "id"}, CodexAgentConflicts: map[string]bool{"child": true},
			Grok: &grokCarry{Tail: checkpoint, Model: "model"}, Droid: &droidCarry{Tail: checkpoint, Model: "model"},
			Kimi: &kimiCarry{Tail: checkpoint}, Agy: &agyCarry{Tail: checkpoint, Steps: map[int]agyCounted{1: {Model: "model"}}},
			Dsh: &dshCarry{Tail: checkpoint, Head: dshHead{ID: "session"}}, Cline: &clineCarry{Prompt: "original"},
		},
	}
}

func changeSessionCacheSnapshotFixture(value *state) {
	value.Cwd, value.Title, value.Named = "/changed", "changed", "changed"
	value.CodexIdentity.Role = "changed"
	value.Total.Input++
	value.Models[0].v.Input++
	daily := value.Days[0].v
	daily.Models[0].v.Input++
	daily.Hours[0]++
	daily.Tools[0].v++
	daily.Skills[0].v++
	for _, values := range [][]string{value.Compacts, value.Convs, value.ResponseIDs, value.SeedConvs, value.TurnConvs} {
		values[0] = "changed"
	}
	value.CodexSpawnNames["child"] = "changed"
	value.CodexSpawnConflicts["child"] = false
	value.CodexAgentIDs["child"] = "changed"
	value.CodexAgentConflicts["child"] = false
	message := value.Claude.Messages["message"]["request"]
	message.Usage.Input++
	message.Previous[0].Input++
	message.Blocks["block"] = false
	tool := message.Tools["tool"]
	tool.Previous[0].Name = "changed"
	message.Tools["tool"] = tool
	value.Grok.Model = "changed"
	value.Droid.Model = "changed"
	value.Kimi.Forked = true
	value.Agy.Last++
	value.Agy.Steps[1] = agyCounted{Model: "changed"}
	value.Dsh.Head.ID = "changed"
	value.Cline.Prompt = "changed"
	for _, checkpoint := range []*tail{value.Grok.Tail, value.Droid.Tail, value.Kimi.Tail, value.Agy.Tail, value.Dsh.Tail} {
		checkpoint.Off++
	}
}

func TestCacheSaveOwnsScheduledState(test *testing.T) {
	setup(test)
	Saved()
	value := sessionCacheSnapshotFixture()
	path := filepath.Join(test.TempDir(), "session.jsonl")
	glance := &state{Title: "pending"}
	entered, release := make(chan struct{}), make(chan struct{})
	var announce, unblock sync.Once
	open := func() { unblock.Do(func() { close(release) }) }
	oldHook, oldVersions := saveHook, parseVersions
	saveHook = func() { announce.Do(func() { close(entered) }); <-release }
	test.Cleanup(func() {
		open()
		Saved()
		saveHook = oldHook
		mu.Lock()
		parseVersions = oldVersions
		mu.Unlock()
	})
	mu.Lock()
	cache = map[string]*state{path: value, "nil": nil, "glance": glance}
	overlaid = map[string]*state{"glance": glance}
	parseVersions = maps.Clone(parseVersions)
	parseVersions["snapshot-test"] = 1
	var expected bytes.Buffer
	err := encodeCache(&expected, cacheFile{Version: cacheVersion, Zone: zone(), Parsers: parseVersions,
		Files: map[string]*state{path: value, "nil": nil}})
	saveCache()
	mu.Unlock()
	if err != nil {
		test.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		test.Fatal("scheduled save did not reach its write barrier")
	}
	mu.Lock()
	changeSessionCacheSnapshotFixture(cache[path])
	parseVersions["snapshot-test"] = 2
	mu.Unlock()
	open()
	Saved()
	actual, err := os.ReadFile(CachePath())
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(actual, expected.Bytes()) {
		test.Fatal("scheduled save borrowed state mutated after the save was queued")
	}
	mu.Lock()
	var next bytes.Buffer
	err = encodeCache(&next, cacheFile{Version: cacheVersion, Zone: zone(), Parsers: parseVersions,
		Files: map[string]*state{path: value, "nil": nil}})
	saveCache()
	mu.Unlock()
	if err != nil {
		test.Fatal(err)
	}
	Saved()
	actual, err = os.ReadFile(CachePath())
	if err != nil || !bytes.Equal(actual, next.Bytes()) || bytes.Equal(actual, expected.Bytes()) {
		test.Fatal("later save lost the updated live state:", err)
	}
}

func TestCacheSnapshotOmitsCodexWindowFromJSON(test *testing.T) {
	setup(test)
	value := sessionCacheSnapshotFixture()
	mu.Lock()
	cache = map[string]*state{"session": value}
	snapshot := savable()["session"]
	mu.Unlock()
	body, err := json.Marshal(snapshot)
	if err != nil || bytes.Contains(body, []byte("transient")) {
		test.Fatal("aggregate JSON retained the separately stored Codex window:", err)
	}
	if value.Codex == nil || value.Codex.Models["transient"] != "model" {
		test.Fatal("disk snapshot changed the live parser state")
	}
}

func TestCacheSnapshotKeepsNilEntriesAndSparseDays(test *testing.T) {
	setup(test)
	value := &state{Days: named[*day]{{"2026-11-01", nil}}}
	snapshot := func() map[string]*state {
		mu.Lock()
		defer mu.Unlock()
		cache = map[string]*state{"nil": nil, "sparse": value}
		return savable()
	}()
	if len(snapshot) != 2 || snapshot["nil"] != nil || snapshot["sparse"] == nil ||
		len(snapshot["sparse"].Days) != 1 || snapshot["sparse"].Days[0].v != nil {
		test.Fatal("snapshot changed nil cache/day entries")
	}
}

func TestCacheSaveOwnsStateBeforeProviderMetadataFill(test *testing.T) {
	_, codex := setup(test)
	Saved()
	path := filepath.Join(codex, "sessions", "bucket", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"session\",\"model_provider\":\"lazy-provider\"}}\n"), 0o600); err != nil {
		test.Fatal(err)
	}
	firstEntered, secondEntered := make(chan struct{}), make(chan struct{})
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	var firstOpen, secondOpen sync.Once
	releaseFirst := func() { firstOpen.Do(func() { close(firstRelease) }) }
	releaseSecond := func() { secondOpen.Do(func() { close(secondRelease) }) }
	oldHook := saveHook
	writes := 0
	saveHook = func() {
		writes++
		switch writes {
		case 1:
			close(firstEntered)
			<-firstRelease
		case 2:
			close(secondEntered)
			<-secondRelease
		}
	}
	test.Cleanup(func() { releaseFirst(); releaseSecond(); Saved(); saveHook = oldHook })
	mu.Lock()
	cache = map[string]*state{path: {Agent: "codex", ID: "session"}}
	saveCache()
	mu.Unlock()
	select {
	case <-firstEntered:
	case <-time.After(10 * time.Second):
		test.Fatal("initial provider snapshot never reached the writer")
	}
	managed := []Managed{{Session: Session{Agent: "codex", ID: "session", Path: path}}}
	mu.Lock()
	codexProviders(managed, map[string][]file{"codex:session": {{agent: "codex", path: path, main: true}}})
	mu.Unlock()
	if managed[0].Provider != "lazy-provider" {
		test.Fatal("real provider metadata fill did not update the managed row")
	}
	releaseFirst()
	select {
	case <-secondEntered:
	case <-time.After(10 * time.Second):
		test.Fatal("provider metadata fill did not schedule the newer save")
	}
	first := savedIndex(test)
	if first.Files[path] == nil || first.Files[path].Provider != "" {
		test.Fatal("earlier snapshot borrowed the later provider metadata")
	}
	releaseSecond()
	Saved()
	latest := savedIndex(test)
	if latest.Files[path] == nil || latest.Files[path].Provider != "lazy-provider" {
		test.Fatal("later save lost the filled provider metadata")
	}
}

func TestCacheSaveDoesNotBorrowConcurrentLiveState(test *testing.T) {
	setup(test)
	Saved()
	value := sessionCacheSnapshotFixture()
	value.Title = strings.Repeat("payload", 1<<17)
	path := filepath.Join(test.TempDir(), "session.jsonl")
	entered, release := make(chan struct{}), make(chan struct{})
	var announce, unblock sync.Once
	open := func() { unblock.Do(func() { close(release) }) }
	oldHook := saveHook
	saveHook = func() { announce.Do(func() { close(entered) }); <-release }
	finished := make(chan struct{})
	test.Cleanup(func() { open(); <-finished; Saved(); saveHook = oldHook })
	mu.Lock()
	cache = map[string]*state{path: value}
	saveCache()
	mu.Unlock()
	go func() {
		defer close(finished)
		<-entered
		open()
		for iteration := range 20000 {
			mu.Lock()
			value.Cwd = fmt.Sprint(iteration)
			mu.Unlock()
		}
	}()
	Saved()
	written := savedIndex(test)
	if written.Files[path] == nil || written.Files[path].Cwd != "/original" {
		test.Fatal("asynchronous cache serialization borrowed the live state")
	}
}

func TestCacheSaveOmitsSourceOwnedState(test *testing.T) {
	setup(test)
	Saved()
	native := sessionCacheSnapshotFixture()
	owned := sessionCacheSnapshotFixture()
	owned.sourceOwned = true
	clone := owned.clone()
	if !clone.sourceOwned {
		test.Fatal("in-memory clone lost its source ownership")
	}
	mu.Lock()
	cache = map[string]*state{"native": native, "owned": clone, "nil": nil}
	saveCache()
	mu.Unlock()
	Saved()
	first := savedIndex(test)
	_, keptNil := first.Files["nil"]
	if len(first.Files) != 2 || !keptNil || first.Files["nil"] != nil || first.Files["owned"] != nil || first.Files["native"] == nil {
		test.Fatal("source-owned state reached the aggregate cache or native/null state was lost")
	}
	if !owned.sourceOwned || !clone.sourceOwned {
		test.Fatal("saving changed live source authority")
	}
	mu.Lock()
	cache["owned"] = &state{Agent: native.Agent, Title: "native replacement"}
	saveCache()
	mu.Unlock()
	Saved()
	latest := savedIndex(test)
	if len(latest.Files) != 3 || latest.Files["owned"] == nil || latest.Files["owned"].Title != "native replacement" || latest.Files["owned"].sourceOwned {
		test.Fatal("the same native path/agent could not be saved without source authority")
	}
}

func BenchmarkCacheScheduledSnapshot(bench *testing.B) {
	mu.Lock()
	previousCache, previousOverlaid := cache, overlaid
	cache, overlaid = make(map[string]*state, 16000), map[string]*state{}
	for index := range 16000 {
		cache[fmt.Sprint(index)] = &state{Agent: "claude", Title: "shared title", Cwd: "/work",
			Models: named[Tokens]{{"model", Tokens{Input: 1}}}, Days: named[*day]{{"2026-11-01", &day{Models: named[Tokens]{{"model", Tokens{Input: 1}}}}}}}
	}
	defer func() { cache, overlaid = previousCache, previousOverlaid; mu.Unlock() }()
	bench.ReportAllocs()
	bench.ResetTimer()
	for range bench.N {
		if len(savable()) != 16000 {
			bench.Fatal("snapshot lost cache entries")
		}
	}
}
