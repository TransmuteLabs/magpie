package extension

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestUsageSnapshotLockBudget(t *testing.T) {
	for _, row := range []struct {
		name        string
		release     bool
		cancel      bool
		wantErr     error
		wantLockErr bool
	}{
		{name: "read_outlives_acquisition_budget", release: true},
		{name: "wait_reaches_acquisition_budget", wantLockErr: true},
		{name: "caller_canceled", cancel: true, wantErr: context.Canceled},
		{name: "caller_deadline", wantErr: context.DeadlineExceeded},
	} {
		t.Run(row.name, func(t *testing.T) {
			manager := NewManager(t.TempDir())
			t.Cleanup(func() { managerReleaseClose(t, manager) })
			catalogTestInstall(t, manager.catalog, catalogTestPackage(t, "usage-module", "1.0.0"))
			release := sync.OnceFunc(holdPrivateStoreTestLock(t, manager.store))
			defer release()

			synctest.Test(t, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				if row.wantErr == context.DeadlineExceeded {
					ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				type result struct {
					read bool
					err  error
				}
				done := make(chan result, 1)
				go func() {
					read := false
					err := manager.withCatalogStoreRead(ctx, func(readCtx context.Context, _ catalogView, _ *os.Root) error {
						read = true
						time.Sleep(2 * time.Second)
						return readCtx.Err()
					})
					done <- result{read, err}
				}()
				synctest.Wait()
				select {
				case got := <-done:
					t.Fatalf("snapshot finished before waiting for the Store: %+v", got)
				default:
				}
				if row.release {
					time.Sleep(filelock.Wait - time.Second)
					release()
				}
				if row.cancel {
					cancel()
				}

				got := <-done
				if got.read != row.release {
					t.Fatalf("snapshot read = %t, want %t: %v", got.read, row.release, got.err)
				}
				if row.wantLockErr {
					var sdkErr *sdk.Error
					if !errors.As(got.err, &sdkErr) || sdkErr.Code != "store_locked" {
						t.Fatalf("snapshot lock budget: %v, want store_locked", got.err)
					}
				} else if !errors.Is(got.err, row.wantErr) {
					t.Fatalf("snapshot returned %v, want %v", got.err, row.wantErr)
				}
			})
		})
	}
}
