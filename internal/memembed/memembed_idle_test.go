package memembed

import (
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/pii/guard"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSemanticRankKeepsModelAfterStaleIdleTimer(test *testing.T) {
	ModelHomeForTest(test, test.TempDir())
	SparseModelForTest(test)
	if err := os.WriteFile(StatePath(), []byte(`{"on":true}`), 0o600); err != nil {
		test.Fatal(err)
	}

	synctest.Test(test, func(bubble *testing.T) {
		vector := make([]float32, guard.EmbedDim)
		vector[0] = 1
		loads := 0
		loadModel = func(string) (embedder, error) {
			loads++
			return testEmbedder(func(string) []float32 { return vector }), nil
		}

		oldStarted := make(chan struct{})
		releaseOld := make(chan struct{})
		oldDone := make(chan struct{})
		currentDone := make(chan struct{}, 1)
		defer close(releaseOld)
		first := true
		previousStart := startIdleTimer
		bubble.Cleanup(func() {
			unload()
			startIdleTimer = previousStart
		})
		startIdleTimer = func(delay time.Duration, expire func()) *time.Timer {
			if first {
				first = false
				return time.AfterFunc(delay, func() {
					close(oldStarted)
					<-releaseOld
					expire()
					close(oldDone)
				})
			}
			return time.AfterFunc(delay, func() {
				expire()
				currentDone <- struct{}{}
			})
		}

		ranker := Bind(nil).Ranker
		if !ranker.Ready() {
			bubble.Fatal("semantic ranker is unavailable")
		}
		request := sdk.MemorySemanticRequest{
			Query:   "When is the release?",
			Records: []sdk.MemorySemanticRecord{{ID: "release", Text: "Packages ship on Tuesdays"}},
		}
		rank := func() {
			bubble.Helper()
			scores := ranker.Rank(request)
			if len(scores) != 1 || scores["release"] != 1 {
				bubble.Fatalf("rank returned %v, want release with score 1", scores)
			}
		}

		rank()
		<-oldStarted
		rank()
		if loads != 1 {
			bubble.Fatalf("renewing the idle timer loaded the model %d times, want 1", loads)
		}
		releaseOld <- struct{}{}
		<-oldDone
		rank()
		if loads != 1 {
			bubble.Fatalf("replaced idle timer caused a reload: loaded %d times, want 1", loads)
		}

		<-currentDone
		rank()
		if loads != 2 {
			bubble.Fatalf("current idle timer did not release the model: loaded %d times, want 2", loads)
		}
	})
}
