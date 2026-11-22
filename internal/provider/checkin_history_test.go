package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

func TestPluginCheckinHistoryAcrossProcesses(t *testing.T) {
	const peerEnv = "MAGPIE_TEST_CHECKIN_HISTORY_PEER"
	const account, other, legacy = "fakeco|fakeco", "fakeco|fakeco#x1", "fakeco|same-name"
	const changedFile = `{"unreadable":`
	type peerCheckin struct {
		Key     string
		At      time.Time
		Outcome string
	}
	if data := os.Getenv(peerEnv); data != "" {
		var peer peerCheckin
		if err := json.Unmarshal([]byte(data), &peer); err != nil {
			t.Fatal(err)
		}
		if peer.Key == "" {
			if err := os.WriteFile(pluginCheckinPath(), []byte(changedFile), 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
		runner := checkinHistoryFixture(peer.Key, peer.At, func(context.Context) WorkBuddyCheckin {
			outcome := peer.Outcome
			if outcome == "" {
				outcome = CheckinClaimed
			}
			return WorkBuddyCheckin{Outcome: outcome, Credit: 53}
		})
		if results := runner.checkinNow(t.Context(), true); len(results) != 1 || !results[0].Asked {
			t.Fatalf("peer did not run the selected check-in: %+v", results)
		}
		state, err := readCheckins(pluginCheckinPath())
		if err != nil || state[peer.Key].Credit != 53 {
			t.Fatalf("peer did not publish the claim: %+v (%v)", state, err)
		}
		return
	}

	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name    string
		peer    peerCheckin
		cancel  bool
		aliases legacyCheckinKeys
		outcome string
		want    map[string]float64
		claimed time.Time
	}{
		{name: "other-account-while-canceled", peer: peerCheckin{Key: other, At: now.Add(time.Minute)}, cancel: true, want: map[string]float64{account: 11, other: 53}},
		{name: "newer-same-account", peer: peerCheckin{Key: account, At: now.Add(time.Minute)}, want: map[string]float64{account: 53}},
		{name: "same-timestamp", peer: peerCheckin{Key: account, At: now}, want: map[string]float64{account: 53}},
		{name: "newer-observation-returns-later", peer: peerCheckin{Key: account, At: now.Add(-time.Minute)}, outcome: CheckinDone, want: map[string]float64{account: 11}, claimed: now.Add(-time.Minute)},
		{name: "earlier-claim-returns-later", peer: peerCheckin{Key: account, At: now.Add(time.Minute), Outcome: CheckinDone}, want: map[string]float64{account: 53}, claimed: now},
		{name: "newer-alias", peer: peerCheckin{Key: legacy, At: now.Add(time.Minute)}, aliases: legacyCheckinKeys{legacy: {account}}, want: map[string]float64{legacy: 53}},
		{name: "file-becomes-unreadable"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			initial := WorkBuddyCheckin{Day: wbCheckinDay(now.Add(-24 * time.Hour)), At: now.Add(-24 * time.Hour), Outcome: CheckinClaimed, Credit: 7}
			initialKey := account
			if len(scenario.aliases) != 0 {
				initialKey = legacy
			}
			writeFile(t, pluginCheckinPath(), map[string]WorkBuddyCheckin{initialKey: initial})
			runner := checkinHistoryFixture(account, now, func(context.Context) WorkBuddyCheckin {
				peer, err := json.Marshal(scenario.peer)
				if err != nil {
					t.Fatal(err)
				}
				peerCtx, stop := context.WithTimeout(t.Context(), 30*time.Second)
				defer stop()
				child := proc.CommandContext(peerCtx, os.Args[0], "-test.run=^TestPluginCheckinHistoryAcrossProcesses$", "-test.count=1")
				child.Env = append(os.Environ(), peerEnv+"="+string(peer))
				if output, err := child.CombinedOutput(); err != nil {
					t.Fatalf("peer could not publish while the first check-in was at the vendor: %v\n%s", err, output)
				}
				if scenario.cancel {
					cancel()
				}
				outcome := scenario.outcome
				if outcome == "" {
					outcome = CheckinClaimed
				}
				return WorkBuddyCheckin{Outcome: outcome, Credit: 11}
			})
			runner.legacy = scenario.aliases
			results := runner.checkinNow(ctx, true)
			if len(results) != 1 || !results[0].Asked || results[0].Credit != 11 {
				t.Fatalf("confirmed reply from the first check-in: %+v", results)
			}
			if scenario.peer.Key == "" {
				if bytes, err := os.ReadFile(pluginCheckinPath()); err != nil || string(bytes) != changedFile {
					t.Fatalf("check-in overwrote an unreadable file: %s (%v)", bytes, err)
				}
				return
			}
			state, err := readCheckins(pluginCheckinPath())
			if err != nil || len(state) != len(scenario.want) {
				t.Fatalf("concurrent check-in lost another entry: %+v (%v)", state, err)
			}
			for key, credit := range scenario.want {
				if got := state[key]; got.Credit != credit {
					t.Errorf("concurrent check-in saved %q with credit %g, want %g: %+v", key, got.Credit, credit, state)
				}
			}
			if !scenario.claimed.IsZero() && !state[account].Claimed.Equal(scenario.claimed) {
				t.Errorf("newer status lost the other process's confirmed claim: %+v", state[account])
			}
		})
	}
}

func TestPluginCheckinHistoryMigratesNewestAlias(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const account, legacy = "fakeco|fakeco", "fakeco|same-name"
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	day := wbCheckinDay(now)
	writeFile(t, pluginCheckinPath(), map[string]WorkBuddyCheckin{
		account: {Day: day, At: now.Add(-2 * time.Minute), Outcome: CheckinDone, Credit: 7},
		legacy:  {Day: day, At: now.Add(-time.Minute), Outcome: CheckinClaimed, Credit: 53},
	})
	runner := checkinHistoryFixture(account, now, func(context.Context) WorkBuddyCheckin {
		t.Fatal("already settled history reached the vendor")
		return WorkBuddyCheckin{}
	})
	runner.legacy = legacyCheckinKeys{legacy: {account}}
	if got := runner.checkinNow(t.Context(), false); len(got) != 1 || got[0].Credit != 53 {
		t.Errorf("newer alias lost before publication: %+v", got)
	}
	state, err := readCheckins(pluginCheckinPath())
	if err != nil || len(state) != 1 || state[account].Credit != 53 {
		t.Errorf("newer alias lost on publication: %+v (%v)", state, err)
	}
}

func TestPluginCheckinHistoryRequiresPublicationLock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const account = "fakeco|fakeco"
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	before := WorkBuddyCheckin{Day: wbCheckinDay(now.Add(-24 * time.Hour)), At: now.Add(-24 * time.Hour), Outcome: CheckinClaimed, Credit: 7}
	writeFile(t, pluginCheckinPath(), map[string]WorkBuddyCheckin{account: before})
	runner := checkinHistoryFixture(account, now, func(context.Context) WorkBuddyCheckin {
		if err := os.Mkdir(pluginCheckinPath()+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		return WorkBuddyCheckin{Outcome: CheckinClaimed, Credit: 11}
	})
	if got := runner.checkinNow(t.Context(), true); len(got) != 1 || !got[0].Asked || got[0].Credit != 11 {
		t.Errorf("lost confirmed vendor response: %+v", got)
	}
	state, err := readCheckins(pluginCheckinPath())
	if err != nil || len(state) != 1 || state[account].Credit != 7 {
		t.Errorf("wrote history without the publication lock: %+v (%v)", state, err)
	}
}

func TestPluginCheckinHistoryMigrationHonorsDeadlineWithWriterHeld(t *testing.T) {
	runner, account, original := checkinHistoryMigrationFixture(t)
	synctest.Test(t, func(t *testing.T) {
		unlock, held := filelock.Lock(runner.path+".lock", 0)
		if !held {
			t.Fatal("could not lock check-in history as a separate writer")
		}
		defer unlock()
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		runner.checkinNow(ctx, false)
		if elapsed := time.Since(start); ctx.Err() != context.DeadlineExceeded || elapsed >= time.Second {
			t.Errorf("migration waited %v for the writer with a 50ms deadline: %v", elapsed, ctx.Err())
		}
	})
	retryCheckinHistoryMigration(t, runner, account, original)
}

func TestPluginCheckinHistoryMigrationHonorsCancellationAtFlush(t *testing.T) {
	runner, account, original := checkinHistoryMigrationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	flushed := cancelHistoryFlush(t, runner.path, cancel)
	runner.checkinNow(ctx, false)
	if !*flushed {
		t.Fatal("migration did not reach the staged file flush")
	}
	retryCheckinHistoryMigration(t, runner, account, original)
}

func TestPluginCheckinHistorySavesConfirmedClaimAfterCancellationAtFlush(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const account = "fakeco|fakeco"
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := checkinHistoryFixture(account, now, func(context.Context) WorkBuddyCheckin {
		return WorkBuddyCheckin{Outcome: CheckinClaimed, Credit: 11}
	})
	flushed := cancelHistoryFlush(t, runner.path, cancel)
	results := runner.checkinNow(ctx, true)
	if !*flushed || ctx.Err() != context.Canceled || len(results) != 1 || !results[0].Asked {
		t.Fatalf("fixture did not return a claim and cancel at the flush: %+v (%v)", results, ctx.Err())
	}
	state, err := readCheckins(runner.path)
	if err != nil || state[account].Credit != 11 || !state[account].Claimed.Equal(now) {
		t.Errorf("canceling at the flush lost the confirmed claim: %+v (%v)", state, err)
	}
}

func checkinHistoryMigrationFixture(t *testing.T) (checkiner, string, []byte) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const account, alias = "fakeco|fakeco", "fakeco|same-name"
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	runner := checkinHistoryFixture(account, now, func(context.Context) WorkBuddyCheckin {
		t.Error("settled alias reached the vendor")
		return WorkBuddyCheckin{}
	})
	runner.legacy = legacyCheckinKeys{alias: {account}}
	writeFile(t, runner.path, map[string]WorkBuddyCheckin{
		alias: {Day: wbCheckinDay(now), At: now.Add(-time.Minute), Outcome: CheckinClaimed, Credit: 7},
	})
	original, err := os.ReadFile(runner.path)
	if err != nil {
		t.Fatal(err)
	}
	return runner, account, original
}

func retryCheckinHistoryMigration(t *testing.T, runner checkiner, account string, original []byte) {
	t.Helper()
	if current, err := os.ReadFile(runner.path); err != nil || !bytes.Equal(current, original) {
		t.Errorf("canceled migration changed the alias file: %s (%v)", current, err)
	}
	runner.checkinNow(t.Context(), false)
	state, err := readCheckins(runner.path)
	if err != nil || len(state) != 1 || state[account].Credit != 7 {
		t.Errorf("retry did not migrate the alias: %+v (%v)", state, err)
	}
}

func checkinHistoryFixture(key string, at time.Time, perform func(context.Context) WorkBuddyCheckin) checkiner {
	plugin := pluginCheckinProvider{ID: "fakeco", Name: "Fixture", recheck: true, accounts: []pluginCheckinAcct{
		{User: key, key: key, On: true, do: perform},
	}}
	return plugin.checkiner(func() time.Time { return at })
}
