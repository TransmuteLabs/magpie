package memdistill

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

func TestReadAllCanBeStoppedWhileListing(t *testing.T) {
	sb := newSandbox(t)
	on(t, "")
	sb.session("pending", time.Hour, "we deploy on Tuesdays", "keep this preference")
	if _, err := change(func(s *State) {
		s.Last, s.Left, s.Read = sb.at.Add(-2*time.Hour), 8, 3
		s.Done = map[string]time.Time{"codex/prior": sb.at.Add(-2 * Back)}
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	original := list
	list = func(ctx context.Context) ([]sessions.Session, error) {
		if !Running() {
			t.Fatal("listing started before Stop was registered")
		}
		Stop()
		if ctx.Err() != context.Canceled {
			t.Fatal("Stop did not cancel active discovery")
		}
		return sb.list, ctx.Err()
	}
	result, err := ReadAll(t.Context(), "a/m", sb.complete)
	if err != nil || result != (Result{}) || len(sb.asked) != 0 || Running() {
		t.Fatalf("stopped discovery reached the model or kept running: %+v %v %v", result, err, sb.asked)
	}
	after, err := os.ReadFile(Path())
	if err != nil || !slices.Equal(after, before) {
		t.Fatalf("stopped discovery changed the previous run: %s, %v", after, err)
	}
	list = original
	result, err = ReadAll(t.Context(), "a/m", sb.complete)
	if err != nil || result.Read != 1 || len(sb.asked) != 1 {
		t.Fatalf("pending session was not read after Stop: %+v %v %v", result, err, sb.asked)
	}
}
