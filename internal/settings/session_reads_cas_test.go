package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filememo"
)

func TestSessionReadsSnapshotBypassesSameStampMemo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"sessionReads":{"asked":true,"agents":{"claude":true }}}`)
	if err := os.WriteFile(Path(), before, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(Path(), old, old); err != nil {
		t.Fatal(err)
	}
	defer filememo.Hold()()
	if !Shared().SessionReads.Reads("claude") {
		t.Fatal("fixture consent missing")
	}
	info, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	// A foreign in-place edit retains size and mtime; the content decides.
	after := bytes.Replace(before, []byte(`"claude":true `), []byte(`"claude":false`), 1)
	if err := os.WriteFile(Path(), after, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(Path(), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := InspectSessionReads()
	if err != nil || current.Reads.Reads("claude") {
		t.Fatal("fresh revision carried cached consent", current, err)
	}
	if err := CompareAndSwapSessionRead(current.Revision, "codex", true); err != nil {
		t.Fatal(err)
	}
	next, err := InspectSessionReads()
	if err != nil || next.Reads.Reads("claude") || !next.Reads.Reads("codex") {
		t.Fatal("CAS lost the fresh neighboring consent decision", next, err)
	}
}

func TestSessionReadsCASPreservesNeighborsAndRejectsABA(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SetSessionReads(func(*SessionReads) *SessionReads {
		return &SessionReads{Asked: true, All: true, Agents: map[string]bool{"opencode": false, "codex": false, "claude": true}}
	}); err != nil {
		t.Fatal(err)
	}
	initial, err := InspectSessionReads()
	if err != nil || initial.Reads.Reads("opencode") || initial.Reads.Generation == "" {
		t.Fatal(initial, err)
	}
	stale := Load()
	if err := CompareAndSwapSessionRead(initial.Revision, "opencode", true); err != nil {
		t.Fatal(err)
	}
	current, err := InspectSessionReads()
	if err != nil || !current.Reads.All || !current.Reads.Asked ||
		current.Reads.Generation == initial.Reads.Generation ||
		!reflect.DeepEqual(current.Reads.Agents, map[string]bool{"opencode": true, "codex": false, "claude": true}) {
		t.Fatal("selected CAS replaced neighboring consent", current, err)
	}
	if err := CompareAndSwapSessionRead(initial.Revision, "opencode", false); !errors.Is(err, ErrSessionReadsChanged) {
		t.Fatal("stale read decision accepted", err)
	}
	if err := CompareAndSwapSessionRead(current.Revision, "opencode", false); err != nil {
		t.Fatal(err)
	}
	again, err := InspectSessionReads()
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareAndSwapSessionRead(again.Revision, "opencode", true); err != nil {
		t.Fatal(err)
	}
	if err := CompareAndSwapSessionRead(current.Revision, "opencode", false); !errors.Is(err, ErrSessionReadsChanged) {
		t.Fatal("revoke/rebind ABA accepted", err)
	}
	restored, err := InspectSessionReads()
	if err != nil || !restored.Reads.Reads("opencode") || restored.Reads.Generation == current.Reads.Generation {
		t.Fatal("restored permission reused the previous generation", restored, err)
	}
	stale.Theme = "dark"
	if err := Save(stale); err != nil {
		t.Fatal(err)
	}
	saved, err := InspectSessionReads()
	if err != nil || !reflect.DeepEqual(saved.Reads, restored.Reads) {
		t.Fatal("saving another setting changed the read decision", saved, err)
	}
}

func TestSessionReadsCASSerializesDecisions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	initial, err := InspectSessionReads()
	if err != nil || initial.Reads != nil {
		t.Fatal(initial, err)
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, agent := range []string{"opencode", "codex"} {
		group.Go(func() {
			<-start
			results <- CompareAndSwapSessionRead(initial.Revision, agent, true)
		})
	}
	close(start)
	group.Wait()
	close(results)
	var success, stale int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrSessionReadsChanged) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("CAS writers did not serialize: success=%d stale=%d", success, stale)
	}
}

func TestSessionReadsCASFailsClosedOnMalformedFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Settings{}); err != nil {
		t.Fatal(err)
	}
	initial, err := InspectSessionReads()
	if err != nil {
		t.Fatal(err)
	}
	broken := []byte(`{"sessionReads":`)
	if err := os.WriteFile(Path(), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectSessionReads(); err == nil {
		t.Fatal("malformed consent treated as absent")
	}
	if err := CompareAndSwapSessionRead(initial.Revision, "opencode", true); err == nil {
		t.Fatal("CAS replaced unreadable consent")
	}
	after, err := os.ReadFile(Path())
	if err != nil || string(after) != string(broken) {
		t.Fatal("failed CAS changed settings", err)
	}
}

func TestSessionReadsCASRejectsExternalChangesDuringOwnerCheck(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "in-place"
		if replace {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if err := SetSessionReads(func(*SessionReads) *SessionReads {
				return &SessionReads{Asked: true, Agents: map[string]bool{"opencode": false}}
			}); err != nil {
				t.Fatal(err)
			}
			initial, err := InspectSessionReads()
			if err != nil {
				t.Fatal(err)
			}
			var external []byte
			err = CompareAndSwapSessionRead(initial.Revision, "opencode", true, func() error {
				body, err := os.ReadFile(Path())
				if err != nil {
					return err
				}
				var current map[string]any
				if err := json.Unmarshal(body, &current); err != nil {
					return err
				}
				current["gatewayMode"] = "on"
				current["sessionReads"] = map[string]any{
					"asked": true, "agents": map[string]bool{"opencode": false, "codex": true},
				}
				external, err = json.Marshal(current)
				if err != nil {
					return err
				}
				target := Path()
				if replace {
					target = filepath.Join(filepath.Dir(target), "external-settings.json")
				}
				if err := os.WriteFile(target, external, 0o600); err != nil {
					return err
				}
				if replace {
					return os.Rename(target, Path())
				}
				return nil
			})
			if !errors.Is(err, ErrSessionReadsChanged) {
				t.Fatalf("CAS accepted a newer external refusal during owner validation: %v", err)
			}
			after, err := os.ReadFile(Path())
			if err != nil || !bytes.Equal(after, external) {
				t.Fatal("rejected CAS changed the external settings", err)
			}
		})
	}
}
