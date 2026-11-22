package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/steady"
	"github.com/yetone/magpie/internal/testenv"
)

func TestCardHistoryWaitRespectsDeadline(t *testing.T) {
	for _, history := range []struct {
		name string
		mu   sync.Locker
		card SubscriptionQuota
	}{
		{"quota", &quotaHistMu, SubscriptionQuota{Provider: "fixture", User: "alice", Windows: []QuotaWindow{{Name: "Weekly", Used: 60}}}},
		{"credits", &creditsMu, wbCard("workbuddy", "alice", 2, 10)},
		{"balance", &balanceHistMu, SubscriptionQuota{Provider: "fixture", User: "alice", Balance: "$20"}},
	} {
		t.Run(history.name, func(t *testing.T) {
			testenv.SetHome(t, t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			history.mu.Lock()
			done := make(chan struct{})
			defer func() {
				history.mu.Unlock()
				<-done
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			go func() {
				defer close(done)
				noteCardHistory(ctx, []SubscriptionQuota{history.card}, time.Now())
			}()
			select {
			case <-done:
				if ctx.Err() != context.DeadlineExceeded {
					t.Error("history stopped before its deadline")
				}
			case <-time.After(2 * time.Second):
				t.Error("history waited for its writer after the caller's deadline")
			}
		})
	}
}

func TestQuotaHistoryCanceledWriteIsRetryable(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	at := time.Now().UTC().Truncate(time.Second)
	quota := SubscriptionQuota{Provider: "fixture", User: "alice", Windows: []QuotaWindow{{Name: "Weekly", Used: 30}}}
	noteQuotaHistory([]SubscriptionQuota{quota}, at)
	before := QuotaHistoryData()
	if len(before) == 0 {
		t.Fatal("initial point was not written")
	}
	path := quotaHistPath()
	key := quotaHistKey(quota.Provider, quota.User)
	quota.Windows[0].Used = 60
	next := at.Add(10 * time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	flushed := cancelHistoryFlush(t, path, cancel)

	noteCardHistory(ctx, []SubscriptionQuota{quota}, next)
	if !*flushed {
		t.Fatal("history never reached the staged file flush")
	}
	if after := QuotaHistoryData(); !bytes.Equal(after, before) {
		t.Errorf("canceled point changed the file: %s", after)
	}
	if points := quotaHistOwn.h[key]["Weekly"]; len(points) != 1 || points[0].At != at || points[0].Left != 70 {
		t.Errorf("canceled point changed the published memo: %+v", points)
	}
	if lastNoted[key] != at {
		t.Errorf("canceled point advanced the note time: %v", lastNoted[key])
	}
	noteQuotaHistory([]SubscriptionQuota{quota}, next)
	points := lineOf(t, QuotaHistories(at, quota.Provider, quota.User), quota.User, "Weekly")
	if len(points) != 1 || !points[0].At.Equal(next) || points[0].Left != 40 {
		t.Errorf("retry did not replace the nearby point: %+v", points)
	}
}

func TestSiblingCardHistoryCanceledBeforeRename(t *testing.T) {
	for _, history := range []struct {
		name string
		path func() string
		card SubscriptionQuota
	}{
		{"credits", creditsPath, wbCard("workbuddy", "alice", 2, 10)},
		{"balance", balanceHistPath, SubscriptionQuota{Provider: "fixture", User: "alice", Balance: "$20"}},
	} {
		t.Run(history.name, func(t *testing.T) {
			testenv.SetHome(t, t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			path := history.path()
			at := time.Now()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			flushed := cancelHistoryFlush(t, path, cancel)
			noteCardHistory(ctx, []SubscriptionQuota{history.card}, at)
			if !*flushed {
				t.Fatal("history never reached the staged file flush")
			}
			if data, err := os.ReadFile(path); !os.IsNotExist(err) {
				t.Errorf("canceled history published %q, err %v", data, err)
			}
			noteCardHistory(t.Context(), []SubscriptionQuota{history.card}, at)
			if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
				t.Errorf("retry did not publish history: %q, %v", data, err)
			}
		})
	}
}

func cancelHistoryFlush(t *testing.T, path string, cancel context.CancelFunc) *bool {
	t.Helper()
	oldSync := steady.Sync
	flushed := false
	steady.Sync = func(f *os.File) error {
		err := oldSync(f)
		if err == nil && filepath.Dir(f.Name()) == filepath.Dir(path) && strings.HasPrefix(filepath.Base(f.Name()), "."+filepath.Base(path)+".") {
			flushed = true
			cancel()
		}
		return err
	}
	t.Cleanup(func() { steady.Sync = oldSync })
	return &flushed
}
