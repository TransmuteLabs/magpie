package provider

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
)

func zcodeProcessContext() plugin.AccountContext {
	return plugin.AccountContext{Scope: "process-scope", Origin: "https://zcode.z.ai", Generation: "login-two", Family: "zai", UserID: "user", Kind: "individual"}
}

func TestZCodeJournalCreatesAndReopens(t *testing.T) {
	isolateRefreshes(t)
	c := zcodeProcessContext()
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		j.Reset.Pending = &zcodeResetClaim{Key: "claim-a", Window: "week", Generation: c.Generation}
		return true, nil
	}); err != nil {
		t.Fatalf("creating journal: %v", err)
	}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		if j.Reset.Pending == nil || j.Reset.Pending.Key != "claim-a" || j.Reset.Pending.Window != "week" || j.Reset.Pending.Generation != c.Generation {
			t.Fatalf("reopened journal lost its pending claim: %+v", j.Reset.Pending)
		}
		return false, nil
	}); err != nil {
		t.Fatalf("reopening journal: %v", err)
	}
}

func TestZCodeJournalProcessChild(t *testing.T) {
	mode := os.Getenv("MAGPIE_NATIVE_JOURNAL_CHILD")
	if mode == "" {
		return
	}
	c := zcodeProcessContext()
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		p := j.Reset.Pending
		if p == nil || p.Key != "claim-one" || p.Generation != "login-one" || p.Baseline != 12 || p.HighWater != 13 {
			t.Fatalf("restart lost the pending claim: %+v", p)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	unlock, err := zcodeFence(c, "reset")
	if mode == "held" {
		if err == nil {
			unlock()
			t.Fatal("another process stole a live reset fence")
		}
		return
	}
	if err != nil {
		t.Fatalf("released fence could not be acquired: %v", err)
	}
	unlock()
}

func TestZCodeJournalSurvivesRestartAndFencesProcesses(t *testing.T) {
	isolateRefreshes(t)
	c := zcodeProcessContext()
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		j.Reset.WeekHigh = 13
		j.Reset.Pending = &zcodeResetClaim{Key: "claim-one", Window: "week", Generation: "login-one", Baseline: 12, HighWater: 13}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	unlock, err := zcodeFence(c, "reset")
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			unlock()
		}
	})
	run := func(mode string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestZCodeJournalProcessChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "MAGPIE_NATIVE_JOURNAL_CHILD="+mode)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v\n%s", mode, err, out)
		}
	}
	run("held")
	unlock()
	locked = false
	run("released")
}

func TestZCodeJournalKeepsSelectionsIndependent(t *testing.T) {
	isolateRefreshes(t)
	a, b := zcodeProcessContext(), zcodeProcessContext()
	a.Scope, b.Scope = "team-project-a", "team-project-b"
	for _, entry := range []struct {
		context plugin.AccountContext
		used    float64
	}{{a, 12}, {b, 34}, {a, 56}} {
		if err := withZCodeJournal(entry.context, func(j *zcodeJournal) (bool, error) {
			j.Reset.FiveHigh = entry.used
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := withZCodeJournal(b, func(j *zcodeJournal) (bool, error) {
		if j.Reset.FiveHigh != 34 {
			t.Errorf("another selection overwrote this one: %+v", j.Reset)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}
