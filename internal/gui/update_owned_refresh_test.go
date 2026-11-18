package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/update"
)

func ownedRefreshWait(t *testing.T, done <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish", operation)
	}
}

func ownedRefreshNotes(u *updater) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		u.notes.Wait()
		close(done)
	}()
	return done
}

func TestUpdateNotesLanguageRefreshKeepsStagedPayload(t *testing.T) {
	feed := newFreshFeed(t)
	wasFresh := freshInstall.Swap(false)
	defer freshInstall.Store(wasFresh)
	u, stop := freshUpdater(t, true)
	defer stop()
	u.inLang("en")
	u.checkByClock()
	ownedRefreshWait(t, ownedRefreshNotes(u), "initial update notes")
	if got := u.json(); got.State != "ready" || got.Latest != "0.1.11" || feed.downloads.Load() != 1 {
		t.Fatalf("initial staging: %+v, downloads=%d", got, feed.downloads.Load())
	}
	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	payload, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	asked := feed.asked.Load()
	feed.latest.Store("0.1.12")
	mux := http.NewServeMux()
	updateRoutes(mux, &quitCount{})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/update/notes?lang=zh", nil))
	ownedRefreshWait(t, ownedRefreshNotes(u), "dialog language refresh")
	if response.Code != http.StatusOK {
		t.Fatalf("notes dialog: %d %s", response.Code, response.Body)
	}
	var dialog struct {
		Latest string `json:"latest"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &dialog); err != nil {
		t.Fatal(err)
	}
	if dialog.Latest != "0.1.11" {
		t.Fatalf("notes dialog named %q, want the staged version 0.1.11", dialog.Latest)
	}
	if feed.asked.Load() <= asked {
		t.Fatal("notes language refresh never asked the fake feed")
	}
	u.mu.Lock()
	after := u.staged
	u.mu.Unlock()
	if got := u.json(); got.State != "ready" || got.Latest != "0.1.11" || after != staged || feed.downloads.Load() != 1 {
		t.Fatalf("notes language refresh replaced staged payload: %+v, staged=%q, want=%q, downloads=%d", got, after, staged, feed.downloads.Load())
	}
	current, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(payload) {
		t.Fatal("notes language refresh changed the staged payload bytes")
	}
}

func TestUpdateNotesLanguageRefreshKeepsStagedPayloadAcrossChannelSwitch(t *testing.T) {
	feed := newFreshFeed(t)
	wasFresh := freshInstall.Swap(false)
	defer freshInstall.Store(wasFresh)
	u, stop := freshUpdater(t, true)
	everyEntered, stableEntered, resume := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var everyOnce, stableOnce, resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	var refreshFeed *httptest.Server
	defer func() {
		release()
		stop()
		if refreshFeed != nil {
			refreshFeed.Close()
		}
	}()
	u.inLang("en")
	feed.latest.Store("0.1.12")
	u.checkByClock()
	ownedRefreshWait(t, ownedRefreshNotes(u), "initial every-version notes")
	if got := u.json(); got.State != "ready" || got.Latest != "0.1.12" || feed.downloads.Load() != 1 {
		t.Fatalf("initial every-version staging: %+v, downloads=%d", got, feed.downloads.Load())
	}
	u.mu.Lock()
	staged, initial := u.staged, *u.latest
	u.mu.Unlock()
	payload, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	newer, stable := initial, initial
	newer.Version, newer.URL = "0.1.13", "https://example.invalid/v0.1.13"
	stable.Version, stable.URL = "0.1.11", "https://example.invalid/v0.1.11"
	refreshFeed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") == settings.UpdateStable {
			stableOnce.Do(func() { close(stableEntered) })
			json.NewEncoder(w).Encode(stable)
			return
		}
		everyOnce.Do(func() { close(everyEntered) })
		select {
		case <-resume:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(newer)
	}))
	t.Setenv("MAGPIE_UPDATE_FEED", refreshFeed.URL)
	u.jsonIn("zh")
	ownedRefreshWait(t, everyEntered, "every-version relang request")
	configuration := settings.Load()
	configuration.UpdateChannel = settings.UpdateStable
	if err := settings.Save(configuration); err != nil {
		t.Fatal(err)
	}
	release()
	ownedRefreshWait(t, ownedRefreshNotes(u), "language refresh across channel switch")
	select {
	case <-stableEntered:
	default:
		t.Fatal("promoted metadata check never asked for the older Stable release")
	}
	u.mu.Lock()
	after := u.staged
	u.mu.Unlock()
	if got := u.json(); got.State != "ready" || got.Latest != "0.1.12" || after != staged || feed.downloads.Load() != 1 {
		t.Fatalf("notes language refresh replaced staged every-version payload after Stable switch: %+v, staged=%q, want=%q, downloads=%d", got, after, staged, feed.downloads.Load())
	}
	current, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(payload) {
		t.Fatal("notes language refresh changed staged every-version payload bytes after Stable switch")
	}
}

func TestUpdateNotesLanguageRefreshFetchesWithoutStagedPayload(t *testing.T) {
	feed := newFreshFeed(t)
	wasFresh := freshInstall.Swap(false)
	defer freshInstall.Store(wasFresh)
	u, stop := freshUpdater(t, true)
	defer stop()
	u.mu.Lock()
	u.state, u.latest, u.notesIn = "found", &update.Release{Version: "0.1.11"}, "en"
	u.mu.Unlock()
	feed.latest.Store("0.1.12")
	u.jsonIn("zh")
	ownedRefreshWait(t, ownedRefreshNotes(u), "language-triggered update")
	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	if got := u.json(); got.State != "ready" || got.Latest != "0.1.12" || staged == "" || feed.downloads.Load() != 1 {
		t.Fatalf("notes language refresh did not fetch newer release without staged payload: %+v, staged=%q, downloads=%d", got, staged, feed.downloads.Load())
	}
	if payload, err := os.ReadFile(staged); err != nil || string(payload) != "new magpie" {
		t.Fatalf("downloaded payload: %q, %v", payload, err)
	}
}

func TestUpdateNotesLanguageRefreshCancellationJoinsPromotedCheck(t *testing.T) {
	feed := newFreshFeed(t)
	u, stop := freshUpdater(t, true)
	owner, cancelOwner := context.WithCancel(u.notesCtx)
	u.notesCtx = owner
	entered := make(chan context.Context, 1)
	returned := make(chan struct{})
	rescue := make(chan struct{})
	wasFresh, wasStable := freshInstall.Load(), latestStable
	latestStable = func(ctx context.Context) (*update.Release, error) {
		defer close(returned)
		entered <- ctx
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-rescue:
			return nil, context.Canceled
		}
	}
	freshInstall.Store(true)
	defer func() {
		latestStable = wasStable
		freshInstall.Store(wasFresh)
	}()
	joined := make(chan struct{})
	var joinOnce sync.Once
	join := func() {
		joinOnce.Do(func() {
			go func() {
				stop()
				close(joined)
			}()
		})
	}
	defer func() {
		cancelOwner()
		close(rescue)
		join()
		ownedRefreshWait(t, joined, "rescued updater cleanup")
	}()
	u.mu.Lock()
	u.state, u.latest, u.notesIn = "found", &update.Release{Version: "0.1.11"}, "en"
	u.mu.Unlock()
	feed.latest.Store("0.1.12")
	u.jsonIn("zh")
	notesDone := ownedRefreshNotes(u)
	var promoted context.Context
	select {
	case promoted = <-entered:
	case <-notesDone:
		t.Fatal("newer release without staged payload never reached the promoted feed lookup")
	case <-time.After(10 * time.Second):
		t.Fatal("language refresh never reached the promoted feed lookup")
	}
	cancelOwner()
	join()
	if err := promoted.Err(); err != context.Canceled {
		t.Errorf("canceling notes owner did not cancel promoted check: %v", err)
		return
	}
	ownedRefreshWait(t, joined, "canceled promoted check join")
	ownedRefreshWait(t, notesDone, "canceled language lookup")
	select {
	case <-returned:
	default:
		t.Fatal("updater stop returned before the promoted lookup released its resources")
	}
	select {
	case <-rescue:
		t.Fatal("promoted lookup needed rescue instead of owner cancellation")
	default:
	}
	if feed.downloads.Load() != 0 {
		t.Fatalf("canceled promoted check downloaded %d assets", feed.downloads.Load())
	}
}
