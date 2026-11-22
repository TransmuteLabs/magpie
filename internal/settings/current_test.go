package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

func currentSettingsFixture(t testing.TB) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeCurrentFixture(t testing.TB, body []byte) {
	t.Helper()
	if err := os.WriteFile(Path(), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkCurrentDiscovery(t testing.TB, want bool) {
	t.Helper()
	if current, err := LoadCurrent(); err != nil || current.NoAccountDiscovery != want {
		t.Fatalf("LoadCurrent discovery off = %t, %v, want %t", current.NoAccountDiscovery, err, want)
	}
	if off, err := AccountDiscoveryOffNow(); err != nil || off != want {
		t.Fatalf("AccountDiscoveryOffNow = %t, %v, want %t", off, err, want)
	}
}

func TestCurrentSettingsTracksSameStampReplacement(t *testing.T) {
	currentSettingsFixture(t)
	before := []byte(`{"noAccountDiscovery":true }`)
	after := []byte(`{"noAccountDiscovery":false}`)
	if len(before) != len(after) {
		t.Fatal("fixture sizes must match")
	}
	writeCurrentFixture(t, before)
	stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(Path(), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	defer filememo.Hold()()
	if !AccountDiscoveryOff() {
		t.Fatal("the ordinary memo must see the initial setting")
	}
	checkCurrentDiscovery(t, true)

	replacement := filepath.Join(Dir(), "replacement")
	if err := os.WriteFile(replacement, after, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := steady.Rename(replacement, Path()); err != nil {
		t.Fatal(err)
	}
	checkCurrentDiscovery(t, false)
	malformed := bytes.Replace(after, []byte("false"), []byte("falsx"), 1)
	writeCurrentFixture(t, malformed)
	if err := os.Chtimes(Path(), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurrent(); err == nil {
		t.Fatal("same-stamp corruption reused validated settings")
	}
	if _, err := AccountDiscoveryOffNow(); err == nil {
		t.Fatal("same-stamp corruption reused discovery policy")
	}
	writeCurrentFixture(t, after)
	if err := os.Chtimes(Path(), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	checkCurrentDiscovery(t, false)
	parsed := Parses()
	for range 5 {
		checkCurrentDiscovery(t, false)
	}
	if got := Parses() - parsed; got != 0 {
		t.Fatalf("unchanged primary was decoded %d more times", got)
	}
}

func TestCurrentSettingsRetriesChangedGeneration(t *testing.T) {
	for _, test := range []struct {
		name        string
		replacement string
		changing    bool
		remove      bool
		calls       int
		wantErr     bool
	}{
		{name: "replacement", replacement: `{"noAccountDiscovery":true}`, calls: 2},
		{name: "malformed", replacement: `{"noAccountDiscovery":tru`, calls: 2, wantErr: true},
		{name: "removed", remove: true, calls: 1, wantErr: true},
		{name: "continuous", replacement: `{}`, changing: true, calls: 3, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			currentSettingsFixture(t)
			writeCurrentFixture(t, []byte(`{}`))
			replacement := filepath.Join(Dir(), "replacement")
			calls := 0
			current, err := readCurrentSettingsWith(func(path string, before os.FileInfo, room []byte) ([]byte, error) {
				calls++
				if calls == 1 || test.changing {
					if test.remove {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.WriteFile(replacement, []byte(test.replacement), 0o600); err != nil {
							t.Fatal(err)
						}
						if err := steady.Rename(replacement, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				return readCurrentSettingsFile(path, before, room)
			})
			if test.wantErr {
				if err == nil || current != nil {
					t.Fatalf("unverified replacement accepted: %v", err)
				}
				if test.changing || test.remove {
					if !errors.Is(err, errSettingsFileChanged) {
						t.Fatalf("disappearance or continued changes lost the reason: %v", err)
					}
				} else if errors.Is(err, errSettingsFileChanged) {
					t.Fatalf("persistent malformed replacement was treated as another change: %v", err)
				}
			} else if err != nil || current == nil || !current.NoAccountDiscovery {
				t.Fatalf("one replacement lost the current discovery setting: %v", err)
			}
			if calls != test.calls {
				t.Fatalf("snapshot called %d times, want %d", calls, test.calls)
			}
		})
	}
}

func TestCurrentSettingsRejectsInvalidPrimaryDespiteBackup(t *testing.T) {
	currentSettingsFixture(t)
	lastgood.Reset()
	t.Cleanup(lastgood.Reset)
	if err := Save(Settings{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Settings{Theme: "light", NoAccountDiscovery: true}); err != nil {
		t.Fatal(err)
	}
	checkCurrentDiscovery(t, true)
	for _, test := range []struct{ name, body string }{
		{"truncated", `{"theme":"light"`},
		{"blank", "\xef\xbb\xbf \n"},
		{"null", "null\n"},
		{"wrong-field-type", `{"noAccountDiscovery":"false"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeCurrentFixture(t, []byte(test.body))
			if _, err := LoadCurrent(); err == nil {
				t.Fatal("LoadCurrent accepted an invalid primary")
			}
			if _, err := AccountDiscoveryOffNow(); err == nil {
				t.Fatal("AccountDiscoveryOffNow accepted an invalid primary")
			}
			if test.name == "truncated" {
				filememo.Forget()
				if got := Load().Theme; got != "dark" {
					t.Fatalf("ordinary recovery theme = %q, want dark", got)
				}
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		if err := os.Truncate(Path(), settingsFileLimit+1); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCurrent(); err == nil {
			t.Fatal("LoadCurrent accepted an oversized primary")
		}
		if _, err := AccountDiscoveryOffNow(); err == nil {
			t.Fatal("AccountDiscoveryOffNow accepted an oversized primary")
		}
	})
	if err := os.Remove(Path()); err != nil {
		t.Fatal(err)
	}
	t.Run("directory", func(t *testing.T) {
		if err := os.Mkdir(Path(), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Remove(Path()); err != nil {
				t.Error(err)
			}
		})
		if _, err := LoadCurrent(); err == nil {
			t.Fatal("LoadCurrent accepted a directory")
		}
		if _, err := AccountDiscoveryOffNow(); err == nil {
			t.Fatal("AccountDiscoveryOffNow accepted a directory")
		}
	})
	t.Run("dangling link", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(Dir(), "missing-settings.json"), Path()); err != nil {
			t.Skip("settings symlinks unavailable:", err)
		}
		t.Cleanup(func() {
			if err := os.Remove(Path()); err != nil {
				t.Error(err)
			}
		})
		if _, err := LoadCurrent(); err == nil {
			t.Fatal("LoadCurrent accepted a dangling primary link")
		}
		if _, err := AccountDiscoveryOffNow(); err == nil {
			t.Fatal("AccountDiscoveryOffNow accepted a dangling primary link")
		}
	})
	checkCurrentDiscovery(t, false)
	if got, err := LoadCurrent(); err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("absent primary did not give the defaults: %v", err)
	}
	writeCurrentFixture(t, []byte("\xef\xbb\xbf"+`{"noAccountDiscovery":true}`))
	checkCurrentDiscovery(t, true)
}

func TestCurrentSettingsReturnsOwnedValues(t *testing.T) {
	currentSettingsFixture(t)
	writeCurrentFixture(t, []byte(`{
		"modelIDs":{"relay/flash":"flash-native"},
		"trayWindows":{"plan":["weekly"]},
		"sessionReads":{"asked":true,"agents":{"codex":true}},
		"codexWarmAts":["6:00","06:00"]
	}`))
	initial, err := LoadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	want := initial.clone()
	initial.ModelIDs["relay/flash"] = "wrong"
	initial.TrayWindows["plan"][0] = "wrong"
	initial.SessionReads.Agents["codex"] = false
	initial.CodexWarmAts[0] = "wrong"
	got, err := LoadCurrent()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("edited result changed a subsequent read: %v", err)
	}
	if got.CodexWarmAt != "06:00" || len(got.CodexWarmAts) != 1 {
		t.Fatalf("current settings lost normalized times: %q", got.CodexWarmAts)
	}
}

func TestCurrentDiscoveryWaitsForFirstSave(t *testing.T) {
	currentSettingsFixture(t)
	atPublication := make(chan struct{})
	release := make(chan struct{})
	resumeWrite := sync.OnceFunc(func() { close(release) })
	syncFile := steady.Sync
	steady.Sync = func(file *os.File) error {
		if strings.HasPrefix(filepath.Base(file.Name()), ".settings.json.") {
			close(atPublication)
			<-release
		}
		return syncFile(file)
	}
	t.Cleanup(func() { steady.Sync = syncFile })

	saved := make(chan error, 1)
	go func() {
		saved <- Save(Settings{NoAccountDiscovery: true})
	}()
	select {
	case <-atPublication:
	case err := <-saved:
		t.Fatalf("save completed without reaching publication: %v", err)
	}

	type discovery struct {
		off bool
		err error
	}
	started := make(chan struct{})
	result := make(chan discovery, 1)
	go func() {
		close(started)
		off, err := AccountDiscoveryOffNow()
		result <- discovery{off, err}
	}()
	<-started
	select {
	case got := <-result:
		resumeWrite()
		<-saved
		t.Fatalf("discovery read a partial settings save: off=%t, err=%v", got.off, got.err)
	case <-time.After(50 * time.Millisecond):
		resumeWrite()
	}
	if err := <-saved; err != nil {
		t.Error(err)
	}
	if got := <-result; !got.off || got.err != nil {
		t.Fatalf("discovery did not read the published setting: off=%t, err=%v", got.off, got.err)
	}
}

func TestCurrentSettingsDiscoveryWarmAllocationsDoNotGrowWithFile(t *testing.T) {
	currentSettingsFixture(t)
	allocations := func(padding int) float64 {
		body := `{"noAccountDiscovery":true}` + strings.Repeat(" ", padding)
		writeCurrentFixture(t, []byte(body))
		stamp := time.Now().Add(-time.Minute)
		if err := os.Chtimes(Path(), stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return testing.AllocsPerRun(40, func() {
			if off, err := AccountDiscoveryOffNow(); !off || err != nil {
				t.Fatal(off, err)
			}
		})
	}
	small, large := allocations(0), allocations(64<<10)
	if large > small+2 {
		t.Fatalf("warm discovery allocations grow with the file: small %.1f, large %.1f", small, large)
	}
}

func BenchmarkCurrentSettings(b *testing.B) {
	currentSettingsFixture(b)
	input := Settings{
		NoAccountDiscovery: true,
		ModelIDs:           map[string]string{},
		ModelEfforts:       map[string][]string{},
		ModelPrices:        map[string]ModelPrice{},
		CodexWarmAts:       []string{"6:00", "12:00"},
	}
	inPrice, outPrice := 0.75, 3.0
	for index := range 300 {
		id := fmt.Sprintf("relay-%02d/long-model-name-%03d", index%15, index)
		input.ModelIDs[id] = fmt.Sprintf("vendor/model-release-%03d", index)
		input.ModelEfforts[id] = []string{"low", "medium", "high"}
		input.ModelPrices[id] = ModelPrice{Input: &inPrice, Output: &outPrice}
	}
	body, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	writeCurrentFixture(b, body)
	stamp := time.Now().Add(-time.Minute)
	if err := os.Chtimes(Path(), stamp, stamp); err != nil {
		b.Fatal(err)
	}
	checkCurrentDiscovery(b, true)
	b.Run("Discovery", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if off, err := AccountDiscoveryOffNow(); !off || err != nil {
				b.Fatal(off, err)
			}
		}
		b.ReportMetric(float64(len(body)), "file-B")
	})
	b.Run("Owned", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if current, err := LoadCurrent(); !current.NoAccountDiscovery || err != nil {
				b.Fatal(current.NoAccountDiscovery, err)
			}
		}
		b.ReportMetric(float64(len(body)), "file-B")
	})
}
