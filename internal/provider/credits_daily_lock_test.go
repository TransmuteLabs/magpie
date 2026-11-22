package provider

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/testenv"
)

func TestDailyCreditsSerializesHistoryWriters(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	at := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.Local)
	later := at.Add(time.Hour)
	day := later.Format("2006-01-02")
	alice := wbCard("workbuddy", "alice", 13, 100)
	bob := wbCard("workbuddy", "bob", 18, 100)
	noteCardHistory(t.Context(), []SubscriptionQuota{
		wbCard("workbuddy", "alice", 10, 100),
		wbCard("workbuddy", "bob", 10, 100),
	}, at)

	path := creditsPath()
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	otherWriter, err := parseCredits(initial)
	if err != nil {
		t.Fatal(err)
	}
	record := otherWriter[creditsKey(bob, "Credits")]
	if record == nil {
		t.Fatal("the initial reading has no entry for Bob")
	}
	record.Last = creditsReading{Used: 18, Total: 100, At: later}
	record.Days = map[string]float64{day: 8}
	committed, err := json.Marshal(otherWriter)
	if err != nil {
		t.Fatal(err)
	}

	completedWhileLocked := false
	synctest.Test(t, func(t *testing.T) {
		unlock, held := filelock.Lock(path+".lock", 0)
		if !held {
			t.Fatal("could not hold the history writer's file lock")
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			noteCardHistory(t.Context(), []SubscriptionQuota{alice}, later)
		}()
		defer func() {
			unlock()
			<-done
		}()
		synctest.Wait()
		select {
		case <-done:
			completedWhileLocked = true
		default:
		}
		if err := writeFileAtomic(path, committed); err != nil {
			t.Fatal(err)
		}
	})

	if completedWhileLocked {
		t.Error("quota refresh completed while the other writer held credits-daily.json.lock")
	}
	cards := withDailyCredits([]SubscriptionQuota{alice, bob}, later)
	for _, want := range []struct {
		user string
		used float64
	}{
		{"alice", 3},
		{"bob", 8},
	} {
		daily := dailyOf(t, cards, want.user)
		if daily == nil || !reflect.DeepEqual(daily.Days, []DayCredits{{Day: day, Used: want.used}}) {
			t.Errorf("%s: history after both writers = %+v, want %g credits", want.user, daily, want.used)
		}
	}
}

func TestDailyCreditsSkipsHistoryLockWithoutFreshCreditWindows(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	at := time.Now()
	kept := wbCard("workbuddy", "alice", 10, 100)
	kept.AsOf = &at
	deferred := wbCard("workbuddy", "alice", 10, 100)
	deferred.ReadDeferred = true
	noteDailyCredits(t.Context(), []SubscriptionQuota{shareCard("cursor", "bob", 20), kept, deferred}, at)
	if _, err := os.Stat(creditsPath() + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("non-credit and kept windows should not open the credit history lock: %v", err)
	}
}

func TestDailyCreditsWriterCancellationPreservesHistory(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	at := time.Now()
	later := at.Add(time.Second)
	first := wbCard("workbuddy", "alice", 10, 100)
	next := wbCard("workbuddy", "alice", 13, 100)
	noteCardHistory(t.Context(), []SubscriptionQuota{first}, at)

	path := creditsPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		unlock, held := filelock.Lock(path+".lock", 0)
		if !held {
			t.Fatal("could not hold the history writer's file lock")
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			noteCardHistory(ctx, []SubscriptionQuota{next}, later)
		}()
		defer func() {
			cancel()
			unlock()
			<-done
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("canceled quota refresh still waits on the held history lock")
		}
	})
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a canceled quota refresh changed daily credit history")
	}
	noteCardHistory(t.Context(), []SubscriptionQuota{next}, later)
	daily := dailyOf(t, withDailyCredits([]SubscriptionQuota{next}, later), "alice")
	if daily == nil || !reflect.DeepEqual(daily.Days, []DayCredits{{Day: later.Local().Format("2006-01-02"), Used: 3}}) {
		t.Fatalf("history after the competing writer released its lock: %+v", daily)
	}
}
