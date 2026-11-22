package extension

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestProviderUsagePreflightStoreReads(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "0")
	for _, size := range []string{"compact", "oversize"} {
		t.Run(size, func(t *testing.T) {
			manager, entry := fixtureManagerAt(t, 4)
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			scope := seedUsageRefreshReceipt(t, manager, entry, size == "oversize")
			store := manager.Store()
			lease, err := manager.Acquire(ctx, scope.Plugin)
			if err != nil {
				t.Fatal(err)
			}
			client := lease.client
			lease.Close()
			directory := filepath.Join(manager.Namespace(), "private", scope.Plugin)
			for _, dirty := range []bool{false, true} {
				name, expectedReads := "clean", 4
				if dirty {
					name, expectedReads = "dirty", 5
					if _, err := store.InvalidateUsage(scope); err != nil {
						t.Fatal(err)
					}
				}
				t.Run(name, func(t *testing.T) {
					stamp, err := store.UsageStampContext(ctx, scope)
					if err != nil || stamp.Dirty != dirty {
						t.Fatalf("initial Usage stamp = %+v, error = %v", stamp, err)
					}
					info, err := os.Stat(filepath.Join(directory, "accounts.json"))
					if err != nil {
						t.Fatal(err)
					}
					if (info.Size() > storeSnapshotLimit) != (size == "oversize") {
						t.Fatalf("unexpected account file size: %d", info.Size())
					}
					before := client.next.Load()
					revision := stamp.Version()
					request := sdk.UsageRequest{Revision: revision, Credentials: json.RawMessage(`null`)}
					if dirty {
						request = sdk.UsageRequest{}
					}
					reads := countUsageAccountReads(t, directory, func() {
						checkUsageWorkerReply(t, ctx, manager, scope, request, revision)
					})
					t.Logf("accounts_bytes=%d completed_reads=%d want=%d", info.Size(), reads, expectedReads)
					if reads != expectedReads {
						t.Errorf("ordinary provider.usage read accounts.json %d times, want %d", reads, expectedReads)
					}
					if client.next.Load() != before+1 {
						t.Fatal("usage did not make exactly one worker call")
					}
					after, err := store.UsageStampContext(ctx, scope)
					if err != nil || after.Revision != stamp.Revision || after.Dirty {
						t.Fatalf("fresh usage did not prove the same clean revision: %+v, %v", after, err)
					}
				})
			}
			before := client.next.Load()
			_, err = manager.Call(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{Revision: "99"}, nil)
			storeTestError(t, err, "usage_stale")
			_, err = manager.Call(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{Credentials: json.RawMessage(`{}`)}, nil)
			if err == nil || !strings.Contains(err.Error(), "usage credentials must come from the selected account") {
				t.Fatalf("expected refusal for caller credentials: %v", err)
			}
			if client.next.Load() != before {
				t.Fatal("rejected usage reached the worker")
			}
		})
	}
}

func TestProviderUsagePreflightLegacyAndUnscoped(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "0")
	t.Run("legacy-private", func(t *testing.T) {
		manager, entry := fixtureManagerAt(t, 3)
		scope := seedUsageRefreshReceipt(t, manager, entry, false)
		checkUsageWorkerReply(t, t.Context(), manager, scope, sdk.UsageRequest{}, "")
	})
	t.Run("unscoped", func(t *testing.T) {
		manager, entry := fixtureManagerAt(t, 4, sdk.AccountAuth)
		scope := sdk.Scope{Plugin: entry.Manifest.ID, Provider: "provider", Session: "fixture-usage-wire"}
		checkUsageWorkerReply(t, t.Context(), manager, scope, sdk.UsageRequest{}, "")
	})
}

func seedUsageRefreshReceipt(t *testing.T, manager *Manager, entry Installed, oversize bool) sdk.Scope {
	t.Helper()
	scope := refreshScope()
	scope.Plugin = entry.Manifest.ID
	store := manager.Store()
	storeTestAccount(t, store, scope)
	refresh := scope
	refresh.Operation, refresh.Attempt = "usage-refresh-receipt", 1
	if _, err := store.claimRefresh(refresh, entry.Revision, "0"); err != nil {
		t.Fatal(err)
	}
	var padding string
	if oversize {
		padding = strings.Repeat("x", storeSnapshotLimit)
	}
	auth, err := json.Marshal(map[string]string{"usageFixture": "rotated", "padding": padding})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.finishRefresh(refresh, &sdk.AuthResult{ID: scope.Account, Credentials: auth}, "completed"); err != nil {
		t.Fatal(err)
	}
	account, err := store.RefreshedAccountContext(t.Context(), scope)
	if err != nil || account.Generation != scope.Generation+1 {
		t.Fatalf("fixture refresh did not publish a successor: generation=%d err=%v", account.Generation, err)
	}
	scope.Generation = account.Generation
	scope.Session = "fixture-usage-wire"
	return scope
}

func checkUsageWorkerReply(t *testing.T, ctx context.Context, manager *Manager, scope sdk.Scope, request sdk.UsageRequest, revision string) {
	t.Helper()
	var result sdk.UsageResult
	if err := manager.Invoke(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, request, &result, nil); err != nil {
		t.Fatal(err)
	}
	if result.Revision != revision || len(result.Windows) != 1 || result.Windows[0].Used != 12 {
		t.Fatalf("unexpected usage reply revision or windows: %+v", result)
	}
}

func countUsageAccountReads(t *testing.T, directory string, run func()) int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	watch, err := unix.InotifyAddWatch(fd, directory, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE)
	if err != nil {
		t.Fatal(err)
	}
	run()
	var events [4096]byte
	var count int
	for {
		size, err := unix.Read(fd, events[:])
		if err == unix.EAGAIN {
			return count
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil || size <= 0 {
			t.Fatalf("read usage Store events: %d bytes, %v", size, err)
		}
		for offset := 0; offset < size; {
			if size-offset < unix.SizeofInotifyEvent {
				t.Fatal("invalid inotify header")
			}
			event := events[offset:]
			nameOffset := offset + unix.SizeofInotifyEvent
			offset = nameOffset + int(binary.NativeEndian.Uint32(event[12:]))
			mask := binary.NativeEndian.Uint32(event[4:])
			if offset > size || mask&unix.IN_Q_OVERFLOW != 0 || int(binary.NativeEndian.Uint32(event)) != watch {
				t.Fatal("invalid or incomplete inotify events")
			}
			if mask == unix.IN_CLOSE_NOWRITE && bytes.Equal(bytes.TrimRight(events[nameOffset:offset], "\x00"), []byte("accounts.json")) {
				count++
			}
		}
	}
}
