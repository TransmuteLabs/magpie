package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installRoutingFixture(test *testing.T, manager *Manager, version string) Installed {
	test.Helper()
	return installRoutingFixtureOwner(test, manager, version, false)
}

func installRoutingFixtureOwner(test *testing.T, manager *Manager, version string, private bool) Installed {
	test.Helper()
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/routingworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build routing fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "routing.fixture", Version: version, Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 4}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Actions:  []sdk.ActionDescriptor{{ID: "rank", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		RoutingStrategies: []sdk.RoutingStrategyDescriptor{{ID: "reverse", Name: "Reverse", Action: "rank", TimeoutMs: 2000,
			Flags: sdk.RoutingFlags{Exact: true, ByUse: true, ByFree: true, ByPlan: true, BySpeed: true, ByPrice: true}}},
	}
	if private {
		manifest.Actions[0].Component = "backend"
		manifest.Services = []sdk.ServiceDescriptor{{
			ID: "backend", Runtime: "exec", Entry: entry, Args: []string{"--routing-service"},
			Contract: sdk.ServiceContract{ID: "routing.backend", Version: 1}, Methods: []string{"rank"}, Start: "lazy",
		}}
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), data, 0600); err != nil {
		test.Fatal(err)
	}
	installed, err := manager.Install(ctx, directory)
	if err != nil {
		test.Fatal(err)
	}
	return installed
}

func TestRoutingPrivateBackendRetainsFullRootAuthority(test *testing.T) {
	manager := NewManager(test.TempDir())
	closeRoutingManager(test, manager)
	entry := installRoutingFixtureOwner(test, manager, "1.0.0", true)
	if manager.Running(entry.Manifest.ID) || len(manager.RoutingStrategies()) != 1 || len(entry.Manifest.RoutingStrategies) != 1 {
		test.Fatal("private routing registration was dropped or metadata started the module")
	}
	trace := filepath.Join(test.TempDir(), "trace")
	setRoutingConfig(test, manager, map[string]any{"mode": "state-write", "trace": trace})
	lease, err := manager.Acquire(test.Context(), entry.Manifest.ID)
	if err != nil {
		test.Fatal("primary routing handshake failed:", err)
	}
	defer lease.Close()
	if len(lease.Manifest().RoutingStrategies) != 0 || len(lease.Manifest().Actions) != 0 {
		test.Fatal("primary handshake exposed another component's routing authority")
	}
	lease.group.mu.Lock()
	cold := lease.group.components["backend"] == nil
	lease.group.mu.Unlock()
	if !cold {
		test.Fatal("private routing helper started before its first action")
	}
	binding, found := manager.RoutingStrategy("routing.fixture/reverse")
	if !found {
		test.Fatal("full root's private routing strategy was not discoverable")
	}
	result, err := manager.OrderRouting(test.Context(), binding, routingFixtureRequest())
	if err != nil || !slices.Equal(result.Order, []int{1, 0}) {
		test.Fatalf("private routing helper did not order: %v %v", result, err)
	}
	lease.group.mu.Lock()
	backend := lease.group.components["backend"]
	ready := backend != nil && backend.client != nil && backend.ready.Component == "backend" &&
		backend.ready.ConfigVersion == lease.ConfigVersion()
	lease.group.mu.Unlock()
	if !ready {
		test.Fatal("routing did not use its root generation's ready private helper")
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: binding.Module}, "routing.must-not-write")
	if err != nil || state.Version != 0 {
		test.Fatal("private routing read gained host state-write authority")
	}
	forged := binding
	forged.Descriptor.Action = "not-declared"
	if _, err := manager.OrderRouting(test.Context(), forged, routingFixtureRequest()); err == nil {
		test.Fatal("filtered public metadata substituted the private root's declared action")
	}
	raw, err := os.ReadFile(trace)
	if err != nil || !strings.Contains(string(raw), `"component":"backend"`) || strings.Count(string(raw), "\n") != 1 {
		test.Fatal("routing execution was not observed in the private helper:", err)
	}
}

func closeRoutingManager(test *testing.T, manager *Manager) {
	test.Helper()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			test.Error(err)
		}
	})
}

func setRoutingConfig(test *testing.T, manager *Manager, options map[string]any) {
	test.Helper()
	current, err := manager.ModuleConfig(test.Context(), "routing.fixture")
	if err != nil {
		test.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), "routing.fixture", current.Version, raw); err != nil {
		test.Fatal(err)
	}
}

func TestRoutingMetadataSynctestDoesNotStartRuntime(test *testing.T) {
	root := test.TempDir()
	writer := NewManager(root)
	closeRoutingManager(test, writer)
	installRoutingFixture(test, writer, "1.0.0")
	manager := NewManager(root)
	closeRoutingManager(test, manager)
	synctest.Test(test, func(bubble *testing.T) {
		if len(manager.RoutingStrategies()) != 1 {
			bubble.Fatal("routing metadata unavailable")
		}
		strategy, found := manager.RoutingStrategy("routing.fixture/reverse")
		flags := strategy.Descriptor.Flags
		if !found || !flags.Exact || !flags.ByPrice || !flags.ByPlan || !flags.BySpeed || !flags.ByFree || !flags.ByUse {
			bubble.Fatal("metadata lost routing flags")
		}
		if manager.startCtx != nil || manager.closeDone != nil || manager.Running("routing.fixture") || len(manager.workers) != 0 {
			bubble.Fatal("pure routing metadata created a lifecycle or worker")
		}
	})
	strategy, _ := manager.RoutingStrategy("routing.fixture/reverse")
	result, err := manager.OrderRouting(test.Context(), strategy, routingFixtureRequest())
	if err != nil || !slices.Equal(result.Order, []int{1, 0}) {
		test.Fatalf("runtime after metadata bubble: %v, %v", result, err)
	}
}

func TestRoutingMetadataHasABoundedCatalogWait(test *testing.T) {
	manager := NewManager(test.TempDir())
	closeRoutingManager(test, manager)
	installRoutingFixture(test, manager, "1.0.0")
	root, err := manager.catalog.open(false)
	if err != nil {
		test.Fatal(err)
	}
	defer root.Close()
	func() {
		unlock, err := catalogLock(test.Context(), root)
		if err != nil {
			test.Fatal(err)
		}
		defer unlock()
		synctest.Test(test, func(bubble *testing.T) {
			begin := time.Now()
			if len(manager.RoutingStrategies()) != 0 {
				bubble.Fatal("locked catalog unexpectedly returned routing metadata")
			}
			if elapsed := time.Since(begin); elapsed > sdk.MaxRoutingTimeoutMs*time.Millisecond {
				bubble.Fatalf("routing list waited %s on the catalog lock", elapsed)
			}
		})
	}()
	if len(manager.RoutingStrategies()) != 1 || manager.Running("routing.fixture") {
		test.Fatal("routing metadata did not recover cold")
	}
}

func TestRoutingReadActionFailureAndCatalogLifecycle(test *testing.T) {
	manager := NewManager(test.TempDir())
	closeRoutingManager(test, manager)
	entry := installRoutingFixture(test, manager, "1.0.0")
	request := routingFixtureRequest()
	trace := filepath.Join(test.TempDir(), "trace.jsonl")
	setRoutingConfig(test, manager, map[string]any{"trace": trace, "mode": "state-write"})
	strategy, _ := manager.RoutingStrategy("routing.fixture/reverse")
	result, err := manager.OrderRouting(test.Context(), strategy, request)
	if err != nil || !slices.Equal(result.Order, []int{1, 0}) {
		test.Fatalf("ordinary read action failed: %v, %v", result, err)
	}
	stored, err := manager.Store().StateGet(sdk.Scope{Plugin: entry.Manifest.ID}, "routing.must-not-write")
	if err != nil || stored.Version != 0 {
		test.Fatal("routing read wrote persistent host state")
	}
	raw, err := os.ReadFile(trace)
	if err != nil || strings.Contains(string(raw), "PRIVATE") || !strings.Contains(string(raw), `"pool":"group/test"`) {
		test.Fatal("routing request authority/projection was not observed")
	}
	for _, mode := range []string{"invalid", "error", "timeout"} {
		setRoutingConfig(test, manager, map[string]any{"mode": mode})
		strategy, _ := manager.RoutingStrategy("routing.fixture/reverse")
		ctx, cancel := context.WithCancel(test.Context())
		if mode == "timeout" {
			cancel()
			ctx, cancel = context.WithTimeout(test.Context(), 100*time.Millisecond)
		}
		_, err := manager.OrderRouting(ctx, strategy, request)
		cancel()
		if err == nil {
			test.Fatalf("%s accepted an invalid order", mode)
		}
		if mode == "invalid" && !strings.Contains(err.Error(), "invalid permutation") {
			test.Fatalf("invalid permutation did not reach validation: %v", err)
		}
	}
	setRoutingConfig(test, manager, map[string]any{})
	old, _ := manager.RoutingStrategy("routing.fixture/reverse")
	other := NewManager(manager.Namespace())
	closeRoutingManager(test, other)
	if err := other.SetEnabled(test.Context(), entry.Manifest.ID, false); err != nil {
		test.Fatal(err)
	}
	if len(manager.RoutingStrategies()) != 0 {
		test.Fatal("another manager's disable was hidden by cached metadata")
	}
	_, staleErr := manager.OrderRouting(test.Context(), old, request)
	if _, found := manager.RoutingStrategy("routing.fixture/reverse"); found || staleErr == nil {
		test.Fatal("revoked routing remained executable")
	}
	if err := other.SetEnabled(test.Context(), entry.Manifest.ID, true); err != nil {
		test.Fatal(err)
	}
	if _, err := manager.OrderRouting(test.Context(), old, request); err == nil {
		test.Fatal("routing snapshot survived disable followed by enable")
	}
	enabled, found := manager.RoutingStrategy("routing.fixture/reverse")
	if !found {
		test.Fatal("re-enabled routing was not discoverable")
	}
	setRoutingConfig(test, manager, map[string]any{"mode": "state-write"})
	if _, err := manager.OrderRouting(test.Context(), enabled, request); err == nil {
		test.Fatal("routing snapshot borrowed new configuration")
	}
	installRoutingFixture(test, other, "2.0.0")
	if _, err := manager.OrderRouting(test.Context(), old, request); err == nil {
		test.Fatal("old routing snapshot borrowed a replacement generation")
	}
	current, found := manager.RoutingStrategy("routing.fixture/reverse")
	result, err = manager.OrderRouting(test.Context(), current, request)
	if !found || err != nil || !slices.Equal(result.Order, []int{0, 1}) {
		test.Fatal("replacement routing generation did not become visible")
	}
}

func routingFixtureRequest() sdk.RoutingRequest {
	return sdk.RoutingRequest{Strategy: "reverse", Pool: "group/test",
		Seats: []sdk.RoutingSeat{{Key: "opaque-a", Weight: 1}, {Key: "opaque-b", Weight: 1}}}
}

func TestRoutingAdmissionRespectsDeadlineWhileOwnerIsLocked(test *testing.T) {
	manager := NewManager(test.TempDir())
	closeRoutingManager(test, manager)
	installRoutingFixture(test, manager, "1.0.0")
	binding, found := manager.RoutingStrategy("routing.fixture/reverse")
	if !found {
		test.Fatal("fixture did not register")
	}
	root, err := manager.catalog.open(false)
	if err != nil {
		test.Fatal(err)
	}
	defer root.Close()
	private, err := storeDirectory(root, "private")
	if err != nil {
		test.Fatal(err)
	}
	defer private.Close()
	stateLock, err := storeOpenLock(private)
	if err != nil {
		test.Fatal(err)
	}
	defer stateLock.Close()
	for _, owner := range []string{"catalog", "private state", "reload"} {
		test.Run(owner, func(test *testing.T) {
			var unlock func()
			switch owner {
			case "catalog":
				unlock, err = catalogLock(test.Context(), root)
				if err != nil {
					test.Fatal(err)
				}
			case "private state":
				if held, err := filelock.TryLock(stateLock); err != nil || !held {
					test.Fatalf("private lock: held=%v, err=%v", held, err)
				}
				unlock = func() { filelock.Unlock(stateLock) }
			case "reload":
				manager.reloadMu.Lock()
				unlock = manager.reloadMu.Unlock
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			ctx, cancel := context.WithTimeout(test.Context(), 100*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := manager.OrderRouting(ctx, binding, routingFixtureRequest())
				result <- err
			}()
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) {
					test.Fatalf("owner did not report the expired request: %v", err)
				}
			case <-time.After(2 * time.Second):
				unlock()
				unlock = nil
				<-result
				test.Fatal("owner waited for lock release past the request deadline")
			}
		})
	}
}

func TestContributionAccountPublicationRespectsDeadline(test *testing.T) {
	manager := NewManager(test.TempDir())
	closeRoutingManager(test, manager)
	account, err := manager.store.SaveAccount(Account{
		Plugin: "search.fixture", Provider: "search", ID: "account", Auth: json.RawMessage(`{"key":"fixture"}`),
	}, 0)
	if err != nil {
		test.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	invocation := &invocationAuthority{scope: scope}
	if err := manager.checkResultPublication(test.Context(), invocation); err != nil {
		test.Fatal(err)
	}
	unlock := holdPrivateStoreTestLock(test, manager.store)
	ctx, cancel := context.WithTimeout(test.Context(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- manager.checkResultPublication(ctx, invocation) }()
	select {
	case err := <-result:
		unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			test.Fatalf("expired publication returned %v", err)
		}
	case <-time.After(2 * time.Second):
		unlock()
		<-result
		test.Fatal("result waited for account authority after the deadline")
	}
}

func routingCatalogChild() {
	args := os.Args[len(os.Args)-2:]
	manager := NewManager(args[0])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if manager.SetEnabled(ctx, args[1], false) != nil || manager.Close(ctx) != nil {
		os.Exit(1)
	}
}

func TestRoutingHeldOrderRejectsCrossProcessRevocation(test *testing.T) {
	testRoutingHeldMutation(test, "disable")
}

func TestRoutingHeldOrderRejectsConfigurationChange(test *testing.T) {
	testRoutingHeldMutation(test, "config")
}

func testRoutingHeldMutation(test *testing.T, mutation string) {
	test.Helper()
	manager := NewManager(test.TempDir())
	closeRoutingManager(test, manager)
	installRoutingFixture(test, manager, "1.0.0")
	marker := filepath.Join(test.TempDir(), "started")
	release := filepath.Join(test.TempDir(), "release")
	setRoutingConfig(test, manager, map[string]any{"mode": "hold", "started": marker, "release": release})
	binding, _ := manager.RoutingStrategy("routing.fixture/reverse")
	finished := make(chan error, 1)
	go func() {
		_, err := manager.OrderRouting(test.Context(), binding, routingFixtureRequest())
		finished <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
waiting:
	for {
		select {
		case <-tick.C:
			if data, _ := os.ReadFile(marker); strings.Contains(string(data), "ordering") {
				break waiting
			}
		case err := <-finished:
			test.Fatalf("order ended before hold: %v", err)
		case <-deadline.C:
			test.Fatal("order did not reach hold")
		}
	}
	if mutation == "disable" {
		child := exec.CommandContext(test.Context(), os.Args[0], "--sdk-routing-disable", manager.Namespace(), binding.Module)
		if output, err := child.CombinedOutput(); err != nil {
			test.Fatalf("catalog child: %v %s", err, output)
		}
	} else {
		other := NewManager(manager.Namespace())
		closeRoutingManager(test, other)
		setRoutingConfig(test, other, map[string]any{})
	}
	if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
		test.Fatal(err)
	}
	select {
	case err := <-finished:
		want := "revoked during ordering"
		if mutation == "config" {
			want = "configuration changed"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			test.Fatalf("in-flight order did not observe %s: %v", mutation, err)
		}
	case <-deadline.C:
		test.Fatal("revoked order did not finish")
	}
}
