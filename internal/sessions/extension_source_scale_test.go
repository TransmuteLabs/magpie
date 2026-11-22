package sessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func boundSourceScale(t testing.TB, count int) string {
	t.Helper()
	_, installed, unbound := installedSourceFixture(t)
	if err := os.Remove(unbound); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".fixture-omp", "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := "session-00000"
	for index := range count {
		id := fmt.Sprintf("session-%05d", index)
		stamp := "2026-11-13T00:00:00Z"
		if id == target {
			stamp = "2026-11-14T00:00:00Z"
		}
		body := fmt.Appendf(nil, `{"id":%q,"cwd":"/work/source","title":"Session","at":%q,"role":"user","text":"test","input":7}`+"\n", id, stamp)
		if err := os.WriteFile(filepath.Join(root, id+".jsonl"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := BindSessionSourceNative(ctx, installed.Manifest.ID, "native-omp", installed.Revision); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSessionSources(ctx); err != nil {
		t.Fatal(err)
	}
	if size := len(sourceView.Load().records); size != count {
		t.Fatalf("prepared %d records, want %d", size, count)
	}
	return target
}

func findScaledSource(t testing.TB, id string) Session {
	t.Helper()
	selected, found := Find("omp", id)
	if !found || selected.source == nil || selected.ID != id || selected.Agent != "omp" ||
		selected.Input != 7 || selected.SourceStatus != "ready" {
		t.Fatalf("Find did not return the selected delegated session: %+v", selected)
	}
	return selected
}

func BenchmarkSourceNativeHistoryScale(b *testing.B) {
	for _, count := range []int{1, 100, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			id := boundSourceScale(b, count)
			selected := findScaledSource(b, id)
			wantCommand := ResumeCommand("omp", id, "/work/source")
			if wantCommand == "" {
				b.Fatal("fixture has no OMP resume command")
			}
			b.Run("Find", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, found := Find("omp", id)
					if !found || got.source != selected.source || got.Input != 7 ||
						len(sourceView.Load().records) != count {
						b.Fatal("warm Find lost the selected record or the prepared corpus")
					}
				}
			})
			b.Run("Resume", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if command := ResumeNow(selected); command != wantCommand {
						b.Fatalf("ResumeNow = %q, want %q", command, wantCommand)
					}
				}
			})
		})
	}
}
