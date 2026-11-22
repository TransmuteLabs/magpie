package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/steady"
)

func TestHistoryWritersKeepConcurrentRecords(t *testing.T) {
	const childMode = "MAGPIE_TEST_HISTORY_WRITER"
	timestamp := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	record := func(method, user string) {
		t.Helper()
		card := SubscriptionQuota{Provider: "fixture", User: user, Windows: []QuotaWindow{{Name: "Weekly", Used: 30}}}
		switch method {
		case "sample":
			noteQuotaHistoryContext(t.Context(), []SubscriptionQuota{card}, timestamp)
		case "merge":
			data, err := json.Marshal(quotaHist{
				quotaHistKey(card.Provider, user): {"Weekly": {{At: timestamp, Left: 70}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := MergeQuotaHistory(data, timestamp); err != nil {
				t.Fatal(err)
			}
		case "balance":
			card.Balance = "$12"
			noteBalanceHistory(t.Context(), []SubscriptionQuota{card}, timestamp)
		default:
			t.Fatalf("unknown history writer %q", method)
		}
	}
	check := func(method, provider, user string) {
		t.Helper()
		if method == "balance" {
			points := readBalanceHist()[quotaHistKey(provider, user)]
			if len(points) != 1 || points[0].Amount != 12 || !points[0].At.Equal(timestamp) {
				t.Errorf("lost %s balance for %s: %+v", provider, user, points)
			}
			return
		}
		history := QuotaHistories(timestamp, provider, user)
		if len(history) != 1 || len(history[0].Lines) != 1 {
			t.Errorf("lost %s quota for %s: %+v", provider, user, history)
			return
		}
		points := history[0].Lines[0].Points
		if len(points) != 1 || points[0].Left != 70 || !points[0].At.Equal(timestamp) {
			t.Errorf("wrong %s quota for %s: %+v", provider, user, points)
		}
	}
	if method := os.Getenv(childMode); method != "" {
		historyPath := quotaHistPath()
		if method == "balance" {
			historyPath = balanceHistPath()
		}
		lock, err := os.OpenFile(historyPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		available, err := filelock.TryLock(lock)
		if err != nil {
			lock.Close()
			t.Fatal(err)
		}
		if available {
			filelock.Unlock(lock)
		}
		lock.Close()
		if !available {
			fmt.Fprintln(os.Stdout, "waiting")
		}
		record(method, "peer")
		check(method, "fixture", "peer")
		if available {
			fmt.Fprintln(os.Stdout, "published")
		}
		return
	}

	for _, scenario := range []struct {
		name, first, second string
	}{
		{"quota-samples", "sample", "sample"},
		{"quota-sample-then-import", "sample", "merge"},
		{"quota-import-then-sample", "merge", "sample"},
		{"quota-imports", "merge", "merge"},
		{"balance-samples", "balance", "balance"},
		{"quota-fold-then-sample", "fold", "sample"},
		{"balance-fold-then-sample", "fold", "balance"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			foldHome(t)
			historyPath := quotaHistPath()
			if scenario.second == "balance" {
				historyPath = balanceHistPath()
			}
			firstProvider, firstUser := "fixture", "local"
			first := func() { record(scenario.first, firstUser) }
			if scenario.first == "fold" {
				providers := []Provider{
					nvidiaCopy("nvidia", "NVIDIA #001", "nvapi-first"),
					nvidiaCopy("nvidia-2", "NVIDIA #002", "nvapi-second"),
				}
				saveAll(t, providers...)
				card := SubscriptionQuota{Provider: "nvidia-2", Balance: "$12", Windows: []QuotaWindow{{Name: "Weekly", Used: 30}}}
				if scenario.second == "balance" {
					noteBalanceHistory(t.Context(), []SubscriptionQuota{card}, timestamp)
				} else {
					noteQuotaHistoryContext(t.Context(), []SubscriptionQuota{card}, timestamp)
				}
				first = func() {
					if count, err := FoldCopies(); err != nil || count != 1 {
						t.Fatalf("folded %d providers: %v", count, err)
					}
				}
				firstProvider, firstUser = "nvidia", "nvidia #002"
			}

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoryWritersKeepConcurrentRecords$", "-test.count=1")
			cmd.Env = append(os.Environ(), childMode+"="+scenario.second)
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { output.Close() })
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			syncFile := steady.Sync
			var stageErr error
			steady.Sync = func(file *os.File) error {
				if err := syncFile(file); err != nil {
					return err
				}
				if filepath.Dir(file.Name()) != filepath.Dir(historyPath) ||
					!strings.HasPrefix(filepath.Base(file.Name()), "."+filepath.Base(historyPath)+".") {
					return nil
				}
				stageErr = cmd.Start()
				if stageErr == nil {
					var status string
					status, stageErr = bufio.NewReader(output).ReadString('\n')
					if stageErr == nil && status != "waiting\n" && status != "published\n" {
						stageErr = fmt.Errorf("unexpected peer state %q", status)
					}
				}
				return stageErr
			}
			t.Cleanup(func() { steady.Sync = syncFile })
			defer func() {
				if cmd.Process != nil && cmd.ProcessState == nil {
					cancel()
					_ = cmd.Wait()
				}
			}()
			first()
			if cmd.Process == nil || stageErr != nil {
				t.Fatalf("local writer did not reach the flush rendezvous: %v", stageErr)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("peer writer: %v\n%s", err, stderr.Bytes())
			}
			check(scenario.second, firstProvider, firstUser)
			check(scenario.second, "fixture", "peer")
		})
	}
}

func TestBalanceHistoryWriterBypassesHeldMemo(t *testing.T) {
	foldHome(t)
	at := time.Now().UTC().Truncate(time.Second)
	path := balanceHistPath()
	peer := SubscriptionQuota{Provider: "fixture", User: "peer", Balance: "$12"}
	noteBalanceHistory(t.Context(), []SubscriptionQuota{peer}, at)
	mtime := at.Add(-time.Hour)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	defer filememo.Hold()()
	key := quotaHistKey(peer.Provider, peer.User)
	if points := balanceHistShared()[key]; len(points) != 1 || points[0].Amount != 12 {
		t.Fatalf("warm balance memo: %+v", points)
	}
	replacement, err := json.Marshal(balanceHist{key: {{At: at, Amount: 15}}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(replacement)) {
		t.Fatalf("replacement must have the same size: %v, %v", info, err)
	}
	next := filepath.Join(t.TempDir(), "balance.json")
	if err := os.WriteFile(next, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	local := SubscriptionQuota{Provider: "fixture", User: "local", Balance: "$10"}
	noteBalanceHistory(t.Context(), []SubscriptionQuota{local}, at)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var history balanceHist
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	if points := history[key]; len(points) != 1 || points[0].Amount != 15 {
		t.Errorf("write resurrected the held memo's balance: %+v", points)
	}
	if points := history[quotaHistKey(local.Provider, local.User)]; len(points) != 1 || points[0].Amount != 10 {
		t.Errorf("local balance not saved: %+v", points)
	}
}

func TestQuotaHistoryImportReturnsFailureUntilPublished(t *testing.T) {
	foldHome(t)
	at := time.Now().UTC().Truncate(time.Second)
	initial := SubscriptionQuota{Provider: "fixture", User: "local", Windows: []QuotaWindow{{Name: "Weekly", Used: 60}}}
	noteQuotaHistoryContext(t.Context(), []SubscriptionQuota{initial}, at)
	before := QuotaHistoryData()
	if len(before) == 0 {
		t.Fatal("initial point was not saved")
	}
	data, err := json.Marshal(quotaHist{"fixture|peer": {"Weekly": {{At: at, Left: 30}}}})
	if err != nil {
		t.Fatal(err)
	}
	fail := errors.New("quota history fixture flush failed")
	syncFile := steady.Sync
	steady.Sync = func(file *os.File) error {
		if filepath.Dir(file.Name()) == filepath.Dir(quotaHistPath()) &&
			strings.HasPrefix(filepath.Base(file.Name()), ".quota-history.json.") {
			return fail
		}
		return syncFile(file)
	}
	t.Cleanup(func() { steady.Sync = syncFile })
	if err := MergeQuotaHistory(data, at); !errors.Is(err, fail) {
		t.Errorf("merge acknowledged an unpublished write: %v", err)
	}
	if after := QuotaHistoryData(); !bytes.Equal(before, after) {
		t.Errorf("failed import changed history: %s", after)
	}
	steady.Sync = syncFile
	if err := MergeQuotaHistory(data, at); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"local", "peer"} {
		if points := lineOf(t, QuotaHistories(at, "fixture", user), user, "Weekly"); len(points) != 1 || !points[0].At.Equal(at) {
			t.Errorf("retry lost %s: %+v", user, points)
		}
	}
}

func TestQuotaHistoryImportDeadlineWhileFileLocked(t *testing.T) {
	foldHome(t)
	at := time.Now().UTC().Truncate(time.Second)
	data, err := json.Marshal(quotaHist{"fixture|peer": {"Weekly": {{At: at, Left: 30}}}})
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		lock := filelock.Mutex{Path: quotaHistMu.Path}
		if !lock.LockChecked() {
			t.Fatal("could not hold the independent writer lock")
		}
		defer lock.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if err := MergeQuotaHistoryContext(ctx, data, at); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("locked import = %v, want its deadline", err)
		}
		if got := QuotaHistoryData(); len(got) != 0 {
			t.Errorf("import published while another writer held the file: %s", got)
		}
	})
	if err := MergeQuotaHistory(data, at); err != nil {
		t.Fatal(err)
	}
	if points := lineOf(t, QuotaHistories(at, "fixture", "peer"), "peer", "Weekly"); len(points) != 1 || points[0].Left != 30 {
		t.Errorf("retry did not publish history: %+v", points)
	}
}
