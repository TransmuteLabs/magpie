package provider

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
)

func foldHome(t *testing.T) string {
	t.Helper()
	isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	return home
}

// nvidiaCopy is one of yuluo_yx's "NVIDIA #NNN": NVIDIA's preset with a
// key of its own, saved a provider each.
func nvidiaCopy(id, name, key string) Provider {
	return Provider{ID: id, Name: name, Key: key, Preset: "nvidia", Icon: "nvidia-color", Catalog: "nvidia",
		Chat: "https://integrate.api.nvidia.com/v1", Models: []string{"z-ai/glm-5.3", "deepseek-ai/deepseek-v4"}}
}

func saveAll(t *testing.T, ps ...Provider) {
	t.Helper()
	for _, p := range ps {
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFoldCopies(t *testing.T) {
	foldHome(t)
	two := nvidiaCopy("nvidia-2", "NVIDIA #002", "nvapi-2")
	two.Off = true
	other := nvidiaCopy("nvidia-4", "NVIDIA #004", "nvapi-4")
	other.Models = []string{"z-ai/glm-5.3"} // a list of its own: kept apart
	shown := nvidiaCopy("nvidia-5", "NVIDIA #005", "nvapi-5")
	saveAll(t, nvidiaCopy("nvidia", "NVIDIA #001", "nvapi-1"), two, nvidiaCopy("nvidia-3", "NVIDIA #003", "nvapi-3"), other, shown,
		// named by the user, alike but for the key: not copies
		Provider{ID: "relay", Name: "Home relay", Key: "a", Chat: "https://relay.example.com/v1", Models: []string{"m"}},
		Provider{ID: "relay-2", Name: "Work relay", Key: "b", Chat: "https://relay.example.com/v1", Models: []string{"m"}},
		Provider{ID: "mistral", Name: "Mistral #0001", Key: "m1", Chat: "https://api.mistral.ai/v1", Models: []string{"mistral-large"}},
		Provider{ID: "mistral-2", Name: "Mistral #0002", Key: "m2", Chat: "https://api.mistral.ai/v1", Models: []string{"mistral-large"}},
	)
	if err := SaveGroup(Group{ID: "top", Name: "Top", Members: []string{"nvidia/z-ai/glm-5.3", "nvidia-2/z-ai/glm-5.3", "nvidia-3/z-ai/glm-5.3", "nvidia-4/z-ai/glm-5.3"},
		Weights: map[string]int{"nvidia/z-ai/glm-5.3": 3, "nvidia-2/z-ai/glm-5.3": 3, "nvidia-3/z-ai/glm-5.3": 3}}); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	// one copy shown to Codex alone is told apart by the user
	s.Visible = map[string][]string{"codex": {"nvidia-5"}}
	s.ModelNames = map[string]string{"nvidia/z-ai/glm-5.3": "GLM", "nvidia-2/z-ai/glm-5.3": "GLM", "nvidia-3/z-ai/glm-5.3": "GLM",
		"nvidia-4/z-ai/glm-5.3": "GLM", "nvidia-5/z-ai/glm-5.3": "GLM"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	balanceHistMu.Lock()
	writeBalanceHist(t.Context(), balanceHist{"nvidia-2|": {{At: time.Now(), Amount: 5}}, "nvidia|": {{At: time.Now(), Amount: 7}}})
	balanceHistMu.Unlock()

	n, err := FoldCopies()
	if err != nil || n != 3 {
		t.Fatalf("folded %d: %v", n, err)
	}
	ps := load().Providers
	var ids []string
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []string{"nvidia", "nvidia-4", "nvidia-5", "relay", "relay-2", "mistral"}) {
		t.Fatalf("providers %v", ids)
	}
	p, _ := Find("nvidia")
	if p.Name != "NVIDIA" || p.KeyName != "NVIDIA #001" || p.Key != "nvapi-1" || p.Off {
		t.Fatalf("folded into %+v", p)
	}
	want := []KeyAccount{{Name: "NVIDIA #002", Key: "nvapi-2", Off: true}, {Name: "NVIDIA #003", Key: "nvapi-3"}}
	if !slices.Equal(p.Keys, want) {
		t.Fatalf("keys %+v", p.Keys)
	}
	// its calls are counted with the one it went into, by its key
	if r := UsageRenames(); r.Of("nvidia-2", time.Now()) != "nvidia" || r.Of("nvidia-3", time.Now()) != "nvidia" {
		t.Fatalf("usage of the folded: %q", r.Of("nvidia-2", time.Now()))
	}
	// an agent still set to the old id reaches it
	if q, m, ok := Resolve("nvidia-3/z-ai/glm-5.3"); !ok || q.ID != "nvidia" || m != "z-ai/glm-5.3" {
		t.Fatalf("resolve %v %q %v", q.ID, m, ok)
	}
	if m, _ := Find("mistral"); m.Name != "Mistral" || len(m.Keys) != 1 || m.Keys[0].Name != "Mistral #0002" {
		t.Fatalf("mistral %+v", m)
	}
	g, _, _ := FindGroup("group/top")
	if !slices.Equal(g.Members, []string{"nvidia/z-ai/glm-5.3", "nvidia-4/z-ai/glm-5.3"}) || g.Weights["nvidia/z-ai/glm-5.3"] != 3 || len(g.Weights) != 1 {
		t.Fatalf("group %+v %v", g.Members, g.Weights)
	}
	s = settings.Load()
	if _, ok := s.ModelNames["nvidia-2/z-ai/glm-5.3"]; ok || s.ModelNames["nvidia/z-ai/glm-5.3"] != "GLM" || s.ModelNames["nvidia-5/z-ai/glm-5.3"] != "GLM" {
		t.Fatalf("model names %v", s.ModelNames)
	}
	h := readBalanceHist()
	if len(h["nvidia|nvidia #002"]) != 1 || len(h["nvidia|nvidia #001"]) != 1 || h["nvidia-2|"] != nil {
		t.Fatalf("balance history %v", h)
	}

	// once is enough
	if n, err := FoldCopies(); n != 0 || err != nil {
		t.Fatalf("again: %d %v", n, err)
	}
}

// The first that is on takes the others in: off, the first is a key that
// is off, and the provider stays on.
func TestFoldCopiesIntoOneOn(t *testing.T) {
	foldHome(t)
	one := nvidiaCopy("nvidia", "NVIDIA #001", "nvapi-1")
	one.Off = true
	saveAll(t, one, nvidiaCopy("nvidia-2", "NVIDIA #002", "nvapi-2"))
	if n, err := FoldCopies(); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	p, err := Find("nvidia-2")
	if err != nil || p.Off || p.Name != "NVIDIA" || !slices.Equal(p.Keys, []KeyAccount{{Name: "NVIDIA #001", Key: "nvapi-1", Off: true}}) {
		t.Fatalf("%+v %v", p, err)
	}
	if q, _ := Find("nvidia"); q.ID != "nvidia-2" {
		t.Fatalf("old id reaches %q", q.ID)
	}
}

// A providers.json that can't be read is never written over.
func TestFoldCopiesUnreadable(t *testing.T) {
	foldHome(t)
	saveAll(t, nvidiaCopy("nvidia", "NVIDIA #001", "nvapi-1"), nvidiaCopy("nvidia-2", "NVIDIA #002", "nvapi-2"))
	b, _ := os.ReadFile(Path())
	bad := append(slices.Clone(b[:len(b)/2]), '}')
	if err := os.WriteFile(Path(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := FoldCopies(); n != 0 || err == nil {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := os.ReadFile(Path()); string(got) != string(bad) {
		t.Fatal("written over")
	}
}

func TestFoldCopiesRetriesHistoryPublication(t *testing.T) {
	for _, scenario := range []struct {
		name                   string
		path                   func() string
		busy, published, moved bool
	}{
		{name: "balance-lock", path: balanceHistPath, busy: true},
		{name: "quota-lock", path: quotaHistPath, busy: true},
		{name: "balance-write", path: balanceHistPath, published: true},
		{name: "quota-write", path: quotaHistPath, published: true, moved: true},
		{name: "providers-write", path: Path},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			at := seedFoldHistories(t)
			seedFoldSettings(t)
			before := foldFileBytes(t, Path())
			path := scenario.path()
			wantCount := 0
			if scenario.published {
				wantCount = 1
			}
			if scenario.busy {
				testFoldWait(t, func(t *testing.T) {
					unlock, held := filelock.Lock(path+".lock", 0)
					if !held {
						t.Fatal("could not hold an independent history lock")
					}
					defer unlock()
					if count, err := FoldCopies(); count != 0 || err == nil || !strings.Contains(err.Error(), filepath.Base(path)+".lock") {
						t.Errorf("busy %s fold = %d, %v", filepath.Base(path), count, err)
					}
				})
			} else {
				failure := errors.New("fold fixture could not flush " + filepath.Base(path))
				syncFile := steady.Sync
				func() {
					defer func() { steady.Sync = syncFile }()
					steady.Sync = func(file *os.File) error {
						if stagedWriteFor(file, path) {
							return failure
						}
						return syncFile(file)
					}
					if count, err := FoldCopies(); count != wantCount || !errors.Is(err, failure) {
						t.Errorf("failed %s fold = %d, %v", filepath.Base(path), count, err)
					}
				}()
			}
			if changed := !bytes.Equal(before, foldFileBytes(t, Path())); changed != scenario.published {
				t.Errorf("failed fold changed provider IDs = %v, want %v", changed, scenario.published)
			}
			checkFoldHistories(t, at, scenario.moved, false)
			if count, err := FoldCopies(); count != 1-wantCount || err != nil {
				t.Errorf("retry fold = %d, %v", count, err)
			}
			checkFoldHistories(t, at, true, true)
			checkFoldSettings(t)
			if count, err := FoldCopies(); count != 0 || err != nil {
				t.Errorf("repeated successful fold = %d, %v", count, err)
			}
			checkFoldHistories(t, at, true, true)
		})
	}
}

func TestFoldCopiesChecksProvidersAfterHistoryWait(t *testing.T) {
	at := seedFoldHistories(t)
	testFoldWait(t, func(t *testing.T) {
		unlock, held := filelock.Lock(quotaHistPath()+".lock", 0)
		if !held {
			t.Fatal("could not hold an independent quota lock")
		}
		defer func() {
			if unlock != nil {
				unlock()
			}
		}()
		result := make(chan error, 1)
		go func() {
			_, err := FoldCopies()
			result <- err
		}()
		synctest.Wait()
		saveErr := Save(Provider{ID: "unrelated", Key: "fixture", Chat: "https://unrelated.invalid"})
		unlock()
		unlock = nil
		err := <-result
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		if !errors.Is(err, errProvidersChanged) {
			t.Errorf("fold after concurrent provider edit = %v", err)
		}
	})
	checkFoldHistories(t, at, false, false)
	if count, err := FoldCopies(); count != 1 || err != nil {
		t.Errorf("retry after provider edit = %d, %v", count, err)
	}
	if _, err := Find("unrelated"); err != nil {
		t.Errorf("unrelated provider edit was lost: %v", err)
	}
	checkFoldHistories(t, at, true, true)
}

func TestFoldCopiesPreservesMalformedHistoryForRepair(t *testing.T) {
	for _, historyPath := range []func() string{balanceHistPath, quotaHistPath} {
		t.Run(filepath.Base(historyPath()), func(t *testing.T) {
			at := seedFoldHistories(t)
			path := historyPath()
			good := foldFileBytes(t, path)
			bad := bytes.Clone(good)
			bad[0] = '!'
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			files := []string{Path(), balanceHistPath(), quotaHistPath()}
			before := make([][]byte, len(files))
			for i, file := range files {
				before[i] = foldFileBytes(t, file)
			}
			if count, err := FoldCopies(); count != 0 || err == nil || !strings.Contains(err.Error(), filepath.Base(path)) {
				t.Errorf("malformed history fold = %d, %v", count, err)
			}
			for i, file := range files {
				if !bytes.Equal(before[i], foldFileBytes(t, file)) {
					t.Errorf("failed fold changed %s", filepath.Base(file))
				}
			}
			if err := os.WriteFile(path, good, 0o600); err != nil {
				t.Fatal(err)
			}
			if count, err := FoldCopies(); count != 1 || err != nil {
				t.Errorf("fold after repair = %d, %v", count, err)
			}
			checkFoldHistories(t, at, true, true)
		})
	}
}

func TestFoldCopiesMergesHistoryInTimeOrder(t *testing.T) {
	now := seedFoldHistories(t)
	const oldKey, newKey, window = "nvidia-2|", "nvidia|nvidia #002", "Weekly"
	edge := now.Add(-balanceShowSpan)
	older, newer, latest := edge.Add(-time.Hour), edge.Add(time.Hour), edge.Add(2*time.Hour)
	alternate := newer.In(time.FixedZone("other zone", 3600))
	balance := balanceHist{
		oldKey: {{At: older, Amount: 10}, {At: newer, Amount: 8}, {At: alternate, Amount: 9}},
		newKey: {{At: newer, Amount: 9}, {At: latest, Amount: 7}},
	}
	quota := quotaHist{
		oldKey: {window: {{At: older, Left: 100}, {At: newer, Left: 90, Start: &older}, {At: alternate, Left: 90}}},
		newKey: {window: {{At: newer, Left: 90}, {At: latest, Left: 70}}},
	}
	balanceHistMu.Lock()
	balanceErr := writeBalanceHist(t.Context(), balance)
	balanceHistMu.Unlock()
	if balanceErr != nil {
		t.Fatal(balanceErr)
	}
	quotaHistMu.Lock()
	quotaErr := writeQuotaHist(t.Context(), quota)
	quotaHistMu.Unlock()
	if quotaErr != nil {
		t.Fatal(quotaErr)
	}
	if count, err := FoldCopies(); count != 1 || err != nil {
		t.Fatalf("fold = %d, %v", count, err)
	}
	trend := balanceTrend(balanceHistShared()[newKey], now)
	wantBalance := []BalancePoint{{At: newer, Amount: 9}, {At: newer, Amount: 8}, {At: latest, Amount: 7}}
	if trend == nil || !slices.EqualFunc(trend.Points, wantBalance, func(first, second BalancePoint) bool {
		return first.At.Equal(second.At) && first.Amount == second.Amount
	}) {
		t.Errorf("balance trend = %+v, want %v", trend, wantBalance)
	}
	history := QuotaHistories(edge, "nvidia", "NVIDIA #002")
	wantQuota := []QuotaPoint{{At: older, Left: 100}, {At: newer, Left: 90}, {At: newer, Left: 90, Start: &older}, {At: latest, Left: 70}}
	if len(history) != 1 || len(history[0].Lines) != 1 ||
		!slices.EqualFunc(history[0].Lines[0].Points, wantQuota, samePoint) {
		t.Errorf("quota history = %+v, want %v including the point before the range", history, wantQuota)
	}
	if len(balanceHistShared()[oldKey]) != 0 || len(QuotaHistories(time.Time{}, "nvidia-2", "")) != 0 {
		t.Error("fold left history under the old provider ID")
	}
}

func TestFoldCopiesKeepsSettingsEditedWhileWaiting(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "unrelated"
		if split {
			name = "different-model-preference"
		}
		t.Run(name, func(t *testing.T) {
			at := seedFoldHistories(t)
			seedFoldSettings(t)
			providers := foldFileBytes(t, Path())
			testFoldWait(t, func(t *testing.T) {
				unlock, held := filelock.Lock(quotaHistPath()+".lock", 0)
				if !held {
					t.Fatal("could not hold an independent quota lock")
				}
				result := make(chan error, 1)
				go func() {
					count, err := FoldCopies()
					if split && count != 0 || !split && count != 1 {
						t.Errorf("fold = %d, %v", count, err)
					}
					result <- err
				}()
				synctest.Wait()
				current := settings.Load()
				current.NoAccountDiscovery = true
				if split {
					current.ModelNames["nvidia-2/z-ai/glm-5.3"] = "Different model name"
				}
				saveErr := settings.Save(current)
				unlock()
				err := <-result
				if saveErr != nil {
					t.Fatal(saveErr)
				}
				if split && err == nil || !split && err != nil {
					t.Errorf("fold after settings edit = %v", err)
				}
			})
			current := settings.Load()
			if !current.NoAccountDiscovery {
				t.Error("fold overwrote the newer NoAccountDiscovery preference")
			}
			if split {
				if current.ModelNames["nvidia-2/z-ai/glm-5.3"] != "Different model name" ||
					!bytes.Equal(providers, foldFileBytes(t, Path())) {
					t.Error("fold erased a preference distinguishing the two providers")
				}
				checkFoldHistories(t, at, false, false)
			} else {
				checkFoldSettings(t)
			}
		})
	}
}

func TestFoldCopiesRetriesSettingsAndProviderPublication(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		path        func() string
		publication int
		published   bool
	}{
		{name: "settings-write", path: settings.Path, publication: 1, published: true},
		{name: "providers-write", path: Path, publication: 1},
		{name: "finish-write", path: Path, publication: 2, published: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			at := seedFoldHistories(t)
			seedFoldSettings(t)
			providers := foldFileBytes(t, Path())
			before := foldFileBytes(t, settings.Path())
			path := scenario.path()
			failure := errors.New("fold fixture could not flush " + filepath.Base(path))
			syncFile := steady.Sync
			wantCount, retryCount := 0, 1
			if scenario.published {
				wantCount, retryCount = 1, 0
			}
			func() {
				defer func() { steady.Sync = syncFile }()
				writes := 0
				steady.Sync = func(file *os.File) error {
					if stagedWriteFor(file, path) {
						writes++
						if writes == scenario.publication {
							return failure
						}
					}
					return syncFile(file)
				}
				if count, err := FoldCopies(); count != wantCount || !errors.Is(err, failure) {
					t.Errorf("failed %s fold = %d, %v, want %d and write error", filepath.Base(path), count, err, wantCount)
				}
			}()
			if changed := !bytes.Equal(providers, foldFileBytes(t, Path())); changed != scenario.published {
				t.Errorf("provider IDs changed = %v, want %v", changed, scenario.published)
			}
			if scenario.name != "finish-write" && !bytes.Equal(before, foldFileBytes(t, settings.Path())) {
				t.Error("failed fold modified the original settings")
			}
			checkFoldHistories(t, at, scenario.published, scenario.published)
			if count, err := FoldCopies(); count != retryCount || err != nil {
				t.Errorf("fold after clearing the write error = %d, %v, want %d and no error", count, err, retryCount)
			}
			checkFoldSettings(t)
			checkFoldHistories(t, at, true, true)
		})
	}
}

func TestFoldCopiesRechecksSettingsBeforePublishing(t *testing.T) {
	seedFoldHistories(t)
	seedFoldSettings(t)
	before := foldFileBytes(t, settings.Path())
	next := bytes.Replace(before, []byte(`"theme": "system"`), []byte(`"theme": "dark"`), 1)
	if bytes.Equal(before, next) {
		t.Fatal("fixture did not edit a setting")
	}
	syncFile := steady.Sync
	func() {
		defer func() { steady.Sync = syncFile }()
		changed := false
		steady.Sync = func(file *os.File) error {
			if !changed && stagedWriteFor(file, settings.Path()) {
				changed = true
				if err := os.WriteFile(settings.Path(), next, 0o600); err != nil {
					return err
				}
			}
			return syncFile(file)
		}
		if count, err := FoldCopies(); count != 1 || err == nil || !changed {
			t.Errorf("fold after a peer edit during publication = %d, %v, edited %v", count, err, changed)
		}
	}()
	if !bytes.Equal(next, foldFileBytes(t, settings.Path())) {
		t.Fatal("fold overwrote the peer settings edit")
	}
	if count, err := FoldCopies(); count != 0 || err != nil {
		t.Errorf("fold recovery = %d, %v", count, err)
	}
	if current := settings.Load(); current.Theme != "dark" {
		t.Errorf("fold recovery lost new theme, got %q", current.Theme)
	}
	checkFoldSettings(t)
}

func TestFoldCopiesKeepsHistoriesWhenPreferencesDivergeDuringPublication(t *testing.T) {
	at := seedFoldHistories(t)
	seedFoldSettings(t)
	providers := foldFileBytes(t, Path())
	before := foldFileBytes(t, settings.Path())
	next := bytes.Replace(before, []byte(`"nvidia-2/z-ai/glm-5.3": "GLM"`), []byte(`"nvidia-2/z-ai/glm-5.3": "Different model name"`), 1)
	if bytes.Equal(before, next) {
		t.Fatal("fixture did not distinguish the two providers")
	}
	syncFile := steady.Sync
	func() {
		defer func() { steady.Sync = syncFile }()
		changed := false
		steady.Sync = func(file *os.File) error {
			if !changed && stagedWriteFor(file, Path()) {
				changed = true
				if err := os.WriteFile(settings.Path(), next, 0o600); err != nil {
					return err
				}
			}
			return syncFile(file)
		}
		if count, err := FoldCopies(); count != 0 || err == nil || !changed {
			t.Errorf("fold after preferences diverged = %d, %v, edited %v", count, err, changed)
		}
	}()
	if !bytes.Equal(providers, foldFileBytes(t, Path())) || !bytes.Equal(next, foldFileBytes(t, settings.Path())) {
		t.Fatal("rejected publication modified provider IDs or the newer settings")
	}
	if count, err := FoldCopies(); count != 0 || err != nil {
		t.Errorf("fold with different preferences = %d, %v", count, err)
	}
	checkFoldHistories(t, at, false, false)
	current := settings.Load()
	current.ModelNames["nvidia-2/z-ai/glm-5.3"] = "GLM"
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	if count, err := FoldCopies(); count != 1 || err != nil {
		t.Errorf("fold after making preferences match again = %d, %v", count, err)
	}
	if count, err := FoldCopies(); count != 0 || err != nil {
		t.Errorf("repeated successful fold = %d, %v", count, err)
	}
	checkFoldHistories(t, at, true, true)
	checkFoldSettings(t)
}

func seedFoldSettings(t *testing.T) {
	t.Helper()
	if err := settings.Save(settings.Settings{
		ModelNames:   map[string]string{"nvidia/z-ai/glm-5.3": "GLM", "nvidia-2/z-ai/glm-5.3": "GLM"},
		PickedModels: map[string][]string{"codex": {"nvidia/z-ai/glm-5.3", "nvidia-2/z-ai/glm-5.3"}},
		Visible:      map[string][]string{"codex": {"nvidia", "nvidia-2"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func checkFoldSettings(t *testing.T) {
	t.Helper()
	current := settings.Load()
	if len(current.ModelNames) != 1 || current.ModelNames["nvidia/z-ai/glm-5.3"] != "GLM" ||
		!slices.Equal(current.PickedModels["codex"], []string{"nvidia/z-ai/glm-5.3"}) ||
		!slices.Equal(current.Visible["codex"], []string{"nvidia"}) {
		t.Errorf("settings after fold: names %v, picks %v, visible %v", current.ModelNames, current.PickedModels, current.Visible)
	}
}

func testFoldWait(t *testing.T, test func(*testing.T)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		edited.Lock()
		prev := edited.ch
		edited.ch = make(chan struct{})
		edited.Unlock()
		defer func() {
			edited.Lock()
			edited.ch = prev
			edited.Unlock()
		}()
		test(t)
	})
}

func seedFoldHistories(t *testing.T) time.Time {
	t.Helper()
	foldHome(t)
	saveAll(t, nvidiaCopy("nvidia", "NVIDIA #001", "nvapi-1"), nvidiaCopy("nvidia-2", "NVIDIA #002", "nvapi-2"))
	at := time.Now().UTC().Truncate(time.Second)
	history := []SubscriptionQuota{
		{Provider: "nvidia-2", Balance: "$12", Windows: []QuotaWindow{{Name: "Weekly", Used: 30}}},
		{Provider: "fixture", User: "unrelated", Balance: "$12", Windows: []QuotaWindow{{Name: "Weekly", Used: 30}}},
	}
	noteBalanceHistory(t.Context(), history, at)
	noteQuotaHistoryContext(t.Context(), history, at)
	return at
}

func checkFoldHistories(t *testing.T, at time.Time, balanceMoved, quotaMoved bool) {
	t.Helper()
	const oldKey, newKey, unrelated = "nvidia-2|", "nvidia|nvidia #002", "fixture|unrelated"
	balance := readBalanceHist()
	quota, err := readQuotaHist()
	if err != nil {
		t.Fatal(err)
	}
	balanceKey, otherBalanceKey := oldKey, newKey
	if balanceMoved {
		balanceKey, otherBalanceKey = newKey, oldKey
	}
	quotaKey, otherQuotaKey := oldKey, newKey
	if quotaMoved {
		quotaKey, otherQuotaKey = newKey, oldKey
	}
	for _, key := range []string{balanceKey, unrelated} {
		if points := balance[key]; len(points) != 1 || points[0].Amount != 12 || !points[0].At.Equal(at) {
			t.Errorf("balance %s = %+v, want one original point", key, points)
		}
	}
	for _, key := range []string{quotaKey, unrelated} {
		if points := quota[key]["Weekly"]; len(points) != 1 || points[0].Left != 70 || !points[0].At.Equal(at) {
			t.Errorf("quota %s = %+v, want one original point", key, points)
		}
	}
	if len(balance[otherBalanceKey]) != 0 || len(quota[otherQuotaKey]) != 0 {
		t.Errorf("unexpected balance at %s or quota at %s", otherBalanceKey, otherQuotaKey)
	}
}

func foldFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// An app that keeps an entry a key (CC Switch) brings in one provider
// with those keys, whatever the entries are called.
func TestImportFoldsAKeyAnEntry(t *testing.T) {
	home := foldHome(t)
	sqliteFixture(t, filepath.Join(home, ".cc-switch", "cc-switch.db"),
		`CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, website_url TEXT, category TEXT, created_at INTEGER, sort_index INTEGER, PRIMARY KEY (id, app_type))`,
		`INSERT INTO providers VALUES ('a','claude','Relay #1','{"env":{"ANTHROPIC_BASE_URL":"https://relay.example.com","ANTHROPIC_AUTH_TOKEN":"sk-a","ANTHROPIC_MODEL":"claude-sonnet-5"}}','','custom',1,0)`,
		`INSERT INTO providers VALUES ('b','claude','Relay #2','{"env":{"ANTHROPIC_BASE_URL":"https://relay.example.com","ANTHROPIC_AUTH_TOKEN":"sk-b","ANTHROPIC_MODEL":"claude-sonnet-5"}}','','custom',2,1)`,
		`INSERT INTO providers VALUES ('c','claude','spare','{"env":{"ANTHROPIC_BASE_URL":"https://relay.example.com","ANTHROPIC_AUTH_TOKEN":"sk-c","ANTHROPIC_MODEL":"claude-sonnet-5"}}','','custom',3,2)`,
		`INSERT INTO providers VALUES ('d','claude','Other','{"env":{"ANTHROPIC_BASE_URL":"https://other.example.com","ANTHROPIC_AUTH_TOKEN":"sk-d","ANTHROPIC_MODEL":"claude-sonnet-5"}}','','custom',4,3)`,
	)
	var picks []AppPick
	for _, r := range []string{"claude/a", "claude/b", "claude/c", "claude/d"} {
		picks = append(picks, AppPick{Source: "cc-switch", Ref: r})
	}
	if added, err := ImportFromApps(picks); err != nil || len(added) != 4 {
		t.Fatalf("%v %v", added, err)
	}
	ps := load().Providers
	if len(ps) != 2 {
		t.Fatalf("%d providers: %+v", len(ps), ps)
	}
	p := ps[0]
	if p.Key != "sk-a" || len(p.Keys) != 2 || p.Keys[0].Key != "sk-b" || p.Keys[0].Name != "Relay #2" || p.Keys[1].Key != "sk-c" || p.Keys[1].Name != "spare" {
		t.Fatalf("%+v", p)
	}
	if ps[1].Key != "sk-d" || len(ps[1].Keys) != 0 {
		t.Fatalf("other %+v", ps[1])
	}
}
