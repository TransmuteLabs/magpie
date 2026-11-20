package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func whileExtensionAccountLockHeld(t *testing.T, root, name string, run func(context.Context) error) error {
	t.Helper()
	unlock, held := filelock.Lock(filepath.Join(root, name), time.Second)
	if !held {
		t.Fatal("could not hold the SDK account owner lock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case err := <-done:
		unlock()
		return err
	case <-time.After(2 * time.Second):
		unlock()
		<-done
		t.Fatal("owner waited for the held lock after the caller deadline")
		return nil
	}
}

func TestExtensionKeepAliveDiscoveryHonorsDeadline(t *testing.T) {
	t.Run("catalog", func(t *testing.T) {
		fixture := keepAliveOwnerFixture(t, nil)
		err := whileExtensionAccountLockHeld(t, fixture.manager.Namespace(), "catalog.lock", func(ctx context.Context) error {
			_, err := extensionSignInsKept(ctx)
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) || fixture.calls.Load() != 0 || fixture.manager.Running(fixture.scope.Plugin) {
			t.Fatalf("canceled renewal discovery: err=%v renewals=%d", err, fixture.calls.Load())
		}
	})
	t.Run("accounts", func(t *testing.T) {
		host := fixtureExtensionProvider(t, true)
		host.manifest.Contract.Version.Minor = 8
		host.manifest.Providers[0].KeepAlive = &sdk.AccountKeepAliveDescriptor{}
		if providers, err := ExtensionProviders(); err != nil || len(providers) != 2 {
			t.Fatalf("ordinary account discovery: len=%d err=%v", len(providers), err)
		}
		err := whileExtensionAccountLockHeld(t, host.namespace, filepath.Join("private", ".lock"), func(ctx context.Context) error {
			_, err := extensionSignInsKept(ctx)
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled account read: err=%v", err)
		}
	})
}

func TestExtensionAccountTaskAdmissionHonorsDeadline(t *testing.T) {
	fixture := newAccountTaskCatalogFixture(t)
	selection := fixture.selection(t)
	t.Run("catalog", func(t *testing.T) {
		err := whileExtensionAccountLockHeld(t, fixture.manager.Namespace(), "catalog.lock", func(ctx context.Context) error {
			_, _, _, _, err := PrepareExtensionAccountTask(ctx, fixture.manager, selection)
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) || fixture.calls.Load() != 0 ||
			fixture.manager.Running(selection.Scope.Plugin) {
			t.Fatalf("canceled task admission: err=%v model requests=%d", err, fixture.calls.Load())
		}
	})
	t.Run("pinned account", func(t *testing.T) {
		_, _, _, check, err := PrepareExtensionAccountTask(t.Context(), fixture.manager, selection)
		if err != nil {
			t.Fatal(err)
		}
		err = whileExtensionAccountLockHeld(t, fixture.manager.Namespace(), filepath.Join("private", ".lock"), check)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled task recheck: err=%v", err)
		}
	})
}

func TestExtensionCheckinDiscoveryHonorsDeadline(t *testing.T) {
	manager, id, trace := checkinOwnerFixture(t, "")
	for _, name := range []string{"catalog.lock", filepath.Join("private", ".lock")} {
		t.Run(name, func(t *testing.T) {
			err := whileExtensionAccountLockHeld(t, manager.Namespace(), name, func(ctx context.Context) error {
				results := CheckInAccount(ctx, pluginCheckinBy(id), pluginCheckinKey(id, "first"))
				if len(results) != 0 {
					return errors.New("canceled check-in resolved an account")
				}
				return nil
			})
			if err != nil || checkinClaims(t, trace) != 0 || manager.Running("checkin.fixture") {
				t.Fatalf("canceled check-in discovery: err=%v", err)
			}
		})
	}
}

func TestExtensionAccountCallAuthReadHonorsDeadline(t *testing.T) {
	manager, _, _ := checkinOwnerFixture(t, "")
	scope := sdk.Scope{Plugin: "checkin.fixture", Provider: "vendor", Account: "first", Generation: 1}
	err := whileExtensionAccountLockHeld(t, manager.Namespace(), filepath.Join("private", ".lock"), func(ctx context.Context) error {
		_, err := manager.Call(ctx, scope.Plugin, sdk.MethodAuthLogout, scope, struct{}{}, nil)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) || manager.Running(scope.Plugin) {
		t.Fatalf("canceled account read before module startup: err=%v", err)
	}
}

func TestExtensionCheckinRefusesCapturedOffAccount(t *testing.T) {
	manager, id, trace := checkinOwnerFixture(t, "")
	captured := extensionCheckinProviders(t.Context())
	if len(captured) != 1 || len(captured[0].accounts) != 1 || !captured[0].accounts[0].On {
		t.Fatal("ordinary check-in did not select the enabled account")
	}
	if err := SetLoginOn(id, "first", false); err != nil {
		t.Fatal(err)
	}
	results := captured[0].checkiner(time.Now).checkinNow(t.Context(), true)
	if len(results) != 1 || results[0].Outcome != CheckinFailed || checkinClaims(t, trace) != 0 ||
		manager.Running("checkin.fixture") {
		t.Fatalf("account Off did not revoke captured check-in authority: %+v", results)
	}
}

func TestExtensionCheckinRechecksAccountAfterPrivateStartup(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	release := filepath.Join(t.TempDir(), "release")
	manager, id, trace := checkinOwnerFixtureWithService(t, started, release, "")
	key := pluginCheckinKey(id, "first")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan []WorkBuddyCheckin, 1)
	go func() { done <- CheckInAccount(ctx, pluginCheckinBy(id), key) }()
	t.Cleanup(func() {
		cancel()
		_ = os.WriteFile(release, []byte("released"), 0600)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("held check-in did not finish")
		}
	})
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("private check-in helper did not reach startup")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := SetLoginOn(id, "first", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("released"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case results := <-done:
		done <- results
		if len(results) != 1 || results[0].Outcome != CheckinFailed || results[0].Operation == nil ||
			results[0].Operation.State != "not-started" || checkinClaims(t, trace) != 0 {
			t.Fatalf("account Off during private startup dispatched a check-in: %+v", results)
		}
	case <-ctx.Done():
		t.Fatal("private check-in did not finish")
	}
	if err := SetLoginOn(id, "first", true); err != nil {
		t.Fatal(err)
	}
	retried := CheckInAccount(t.Context(), pluginCheckinBy(id), key)
	if len(retried) != 1 || retried[0].Outcome != CheckinClaimed || checkinClaims(t, trace) != 1 ||
		!manager.Running("checkin.fixture") {
		t.Fatalf("definite refusal prevented an authorized retry: %+v", retried)
	}
}
