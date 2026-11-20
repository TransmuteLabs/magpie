package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func projectedCatalog() sdk.CatalogReadResult {
	return sdk.CatalogReadResult{ReadAt: 1, Accounts: []sdk.CatalogAccountProjection{{
		Ref: strings.Repeat("a", 64), Provider: "vendor", Kind: "account",
		Models: []sdk.CatalogModelProjection{{ID: "vendor/model", ContextWindow: 256000}},
	}}}
}

func invokeCatalog(t *testing.T, manager *Manager, scope sdk.Scope) (sdk.CatalogReadResult, error) {
	t.Helper()
	var result sdk.CatalogReadResult
	err := manager.Invoke(t.Context(), scope.Plugin, sdk.MethodToolCall, scope,
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"catalog"}`)}, &result, nil)
	return result, err
}

func TestCatalogProjectionOrdinaryWorkerUsesDeclaredAccount(t *testing.T) {
	manager, scope := usageManagerVersion(t, 8, func(manifest *sdk.Manifest) {
		manifest.CatalogAccess = &sdk.CatalogAccessDescriptor{Scope: "selected"}
	})
	calls := 0
	manager.catalogReader = func(ctx context.Context, selected sdk.Scope, access sdk.CatalogAccessDescriptor) (sdk.CatalogReadResult, error) {
		calls++
		if selected.Plugin != scope.Plugin || selected.Provider != scope.Provider ||
			selected.Account != scope.Account || selected.Generation != scope.Generation || access.Scope != "selected" {
			t.Error("catalog identity came from module input instead of the selected parent", selected, access)
		}
		return projectedCatalog(), ctx.Err()
	}
	result, err := invokeCatalog(t, manager, scope)
	if err != nil || calls != 1 || len(result.Accounts) != 1 || result.Accounts[0].Models[0].ContextWindow != 256000 {
		t.Fatal("ordinary worker could not read its declared sanitized catalog", result, err, calls)
	}
}

func TestCatalogProjectionRefusesUndeclaredRootStaleAndUnknownReads(t *testing.T) {
	t.Run("undeclared", func(t *testing.T) {
		manager, scope := usageManagerVersion(t, 8)
		manager.catalogReader = func(context.Context, sdk.Scope, sdk.CatalogAccessDescriptor) (sdk.CatalogReadResult, error) {
			t.Error("undeclared module reached catalog owner")
			return projectedCatalog(), nil
		}
		if _, err := invokeCatalog(t, manager, scope); err == nil {
			t.Fatal("module acquired catalog access from its ordinary capability")
		}
	})
	for _, mode := range []string{"root", "account replacement", "disabled", "config replacement", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			manager, scope := usageManagerVersion(t, 8, func(manifest *sdk.Manifest) {
				manifest.CatalogAccess = &sdk.CatalogAccessDescriptor{Scope: "selected"}
			})
			manager.catalogReader = func(ctx context.Context, _ sdk.Scope, _ sdk.CatalogAccessDescriptor) (sdk.CatalogReadResult, error) {
				switch mode {
				case "root":
					t.Error("root scope acquired selected-account catalog")
				case "account replacement":
					if _, err := manager.Store().SaveAccount(Account{Plugin: scope.Plugin, Provider: scope.Provider,
						ID: scope.Account, Auth: json.RawMessage(`{"access":"replacement"}`)}, scope.Generation); err != nil {
						t.Error(err)
						return sdk.CatalogReadResult{}, err
					}
				case "disabled":
					if err := manager.catalog.SetEnabled(ctx, scope.Plugin, false); err != nil {
						t.Error(err)
						return sdk.CatalogReadResult{}, err
					}
				case "config replacement":
					if _, err := manager.CompareAndSwapModuleConfig(ctx, scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
						t.Error(err)
						return sdk.CatalogReadResult{}, err
					}
				case "unknown":
					return sdk.CatalogReadResult{}, errors.New("private diagnostic must not be exposed")
				}
				return projectedCatalog(), nil
			}
			if mode == "root" {
				scope.Account, scope.Generation = "", 0
			}
			result, err := invokeCatalog(t, manager, scope)
			if err == nil || len(result.Accounts) != 0 || strings.Contains(err.Error(), "private diagnostic") {
				t.Fatal("revoked or unknown catalog read published data or private failure text", result, err)
			}
		})
	}
}

func TestActiveGenerationChecksStopAtCatalogDeadline(t *testing.T) {
	manager, scope := usageManagerVersion(t, 8, func(manifest *sdk.Manifest) {
		manifest.CatalogAccess = &sdk.CatalogAccessDescriptor{Scope: "selected"}
	})
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lock, err := os.OpenFile(filepath.Join(manager.catalog.Root, "catalog.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, projection := range []bool{false, true} {
		name := "lease"
		if projection {
			name = "projection"
		}
		t.Run(name, func(t *testing.T) {
			lockCatalog := func() error {
				held, err := filelock.TryLock(lock)
				if err == nil && !held {
					return errors.New("could not hold catalog lock")
				}
				return err
			}
			release := sync.OnceFunc(func() { filelock.Unlock(lock) })
			defer release()
			ready := make(chan error, 1)
			manager.catalogReader = func(context.Context, sdk.Scope, sdk.CatalogAccessDescriptor) (sdk.CatalogReadResult, error) {
				err := lockCatalog()
				ready <- err
				return projectedCatalog(), err
			}
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if projection {
					authority := hostAuthority{manifest: lease.Manifest(), method: sdk.MethodToolCall}
					call := lease.group.withAuthority(ctx, sdk.PrimaryComponent, "read")
					_, err := manager.readCatalogProjection(call, scope, json.RawMessage(`{}`), authority)
					result <- err
					return
				}
				err := lockCatalog()
				ready <- err
				if err != nil {
					result <- err
					return
				}
				result <- lease.CheckCurrent(ctx)
			}()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case err := <-result:
				t.Fatalf("generation check returned before reaching the held lock: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("generation check did not reach the lock")
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("generation check returned %v instead of its deadline", err)
				}
			case <-time.After(900 * time.Millisecond):
				release()
				<-result
				t.Fatal("canceled generation check waited for the catalog lock")
			}
		})
	}
}
