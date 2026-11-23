package sessions

import (
	"context"
	"strconv"
	"testing"
)

const scaleStorageWriter = `
from pathlib import Path
import sys

script, directory, rows = sys.argv[1:]
source = Path(script).read_text()
anchor = '        elif command == "migrate":\n'
if source.count(anchor) != 1:
    raise ValueError("expected one migrate branch in the SQLite fixture writer")
seed = '''            writer.executemany(
                "INSERT INTO session_v2(id,project_id,slug,directory,title,version,time_created,time_updated) VALUES(?,'global',?,'/fixture','Scale row','2.3.0',?,?)",
                ((f"ses_scale_{index:05d}", f"scale_{index:05d}", 1794500000000 + index, 1794500000000 + index)
                 for index in range(scale_rows - 1)),
            )
'''
source = source.replace(anchor, anchor + seed)
sys.argv[:] = [script, directory]
exec(compile(source, script, "exec"), {"__file__": script, "__name__": "__main__", "scale_rows": int(rows)})
`

func scaleStorageFixture(t testing.TB, count int) (*storageNativeFixture, storageWriterState) {
	t.Helper()
	fixture := startStorageNativeFixture(t, count)
	before := fixture.ask(t, "migrate")
	if before.DB == "" || before.WAL == "" || before.SHM == "" ||
		before.DB != fixture.initial.DB || before.WAL == fixture.initial.WAL {
		t.Fatalf("migration and %d-row seed must extend the WAL while keeping the main DB intact: %+v", count, before)
	}
	return fixture, before
}

func requireStorageScaleRecord(t testing.TB, ctx context.Context, count int) Session {
	t.Helper()
	selected, found := FindContext(ctx, "opencode", "ses_wal")
	if !found || selected.source == nil || selected.source.storage == nil ||
		selected.Title != "Committed WAL session" || selected.Input != 7 {
		t.Fatalf("ordinary Find lost the WAL session: %+v", selected)
	}
	if view := sourceView.Load(); view == nil || len(view.records) != count {
		t.Fatalf("ordinary Find did not publish all %d SQLite records", count)
	}
	return selected
}

func BenchmarkStorageNativeSQLiteHistoryScale(b *testing.B) {
	for _, count := range []int{1, 100, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			fixture, before := scaleStorageFixture(b, count)
			selected := requireStorageScaleRecord(b, b.Context(), count)
			const wantResume = "cd '/fixture' && opencode --session ses_wal"
			if selected.Resume != wantResume {
				b.Fatalf("listed resume = %q, want %q", selected.Resume, wantResume)
			}
			pages := 0
			sourceBeforeRPC = func(context.Context) error {
				pages++
				return nil
			}
			b.Cleanup(func() { sourceBeforeRPC = nil })
			b.Run("Find", func(b *testing.B) {
				pages = 0
				b.ReportAllocs()
				for b.Loop() {
					if fresh := requireStorageScaleRecord(b, b.Context(), count); fresh.source != selected.source {
						b.Fatal("warm Find replaced the unchanged selected record")
					}
				}
				b.ReportMetric(float64(pages)/float64(b.N), "storage-pages/op")
			})
			b.Run("MissingFind", func(b *testing.B) {
				pages = 0
				b.ReportAllocs()
				for b.Loop() {
					if _, found := FindContext(b.Context(), "opencode", "ses_missing"); found {
						b.Fatal("unknown SQLite session was returned")
					}
					if view := sourceView.Load(); view == nil || len(view.records) != count {
						b.Fatal("missing session lookup lost the prepared catalog")
					}
				}
				b.ReportMetric(float64(pages)/float64(b.N), "storage-pages/op")
			})
			b.Run("Resume", func(b *testing.B) {
				pages = 0
				b.ReportAllocs()
				for b.Loop() {
					if command := ResumeNowContext(b.Context(), selected); command != wantResume {
						b.Fatalf("ResumeNow = %q, want %q", command, wantResume)
					}
				}
				b.ReportMetric(float64(pages)/float64(b.N), "storage-pages/op")
			})
			after := fixture.ask(b, "state")
			if after.DB != before.DB || after.WAL != before.WAL || after.SHM != before.SHM {
				b.Fatal("ordinary Find or Resume changed the donor DB/WAL/SHM bytes")
			}
		})
	}
}

func TestStorageNativeSQLiteFindAndResumeAuthority(t *testing.T) {
	fixture, before := scaleStorageFixture(t, 1)
	selected := requireStorageScaleRecord(t, t.Context(), 1)
	const wantResume = "cd '/fixture' && opencode --session ses_wal"
	if command := ResumeNowContext(t.Context(), selected); command != wantResume {
		t.Fatalf("ResumeNow = %q, want %q", command, wantResume)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, found := FindContext(ctx, "opencode", selected.ID); found {
		t.Fatal("canceled Find returned a SQLite selection")
	}
	if command := ResumeNowContext(ctx, selected); command != "" {
		t.Fatal("canceled ResumeNow returned a SQLite resume command")
	}

	cycled := false
	sourceBeforePublish = func() {
		if !cycled {
			cycled = true
			cycleSourceModule(t, fixture.manager, fixture.entry.Manifest.ID, "MAGPIE_STORAGE_TEST_HOST")
			if command := ResumeNowContext(t.Context(), selected); command != "" {
				t.Fatal("a foreign off/on cycle retained a held SQLite command")
			}
		}
	}
	t.Cleanup(func() { sourceBeforePublish = nil })
	during, found := FindContext(t.Context(), "opencode", selected.ID)
	if !cycled || found && (during.source == nil || during.source == selected.source ||
		during.source.grant.enableGeneration == selected.source.grant.enableGeneration) {
		t.Fatal("Find published a result selected before the foreign enablement cycle")
	}
	sourceBeforePublish = nil
	fresh := requireStorageScaleRecord(t, t.Context(), 1)
	if fresh.source == selected.source || ResumeNowContext(t.Context(), fresh) != wantResume {
		t.Fatal("fresh discovery failed to renew the SQLite selection")
	}
	if command := ResumeNowContext(t.Context(), selected); command != "" {
		t.Fatal("fresh discovery revived a superseded SQLite command")
	}
	after := fixture.ask(t, "state")
	if after.DB != before.DB || after.WAL != before.WAL || after.SHM != before.SHM {
		t.Fatal("foreign enablement changed the SQLite donor bytes")
	}
}
