package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func checkinActivationFixture(t *testing.T, private bool) (*Manager, Installed, string, string, string) {
	t.Helper()
	return checkinActivationFixtureWithMinor(t, private, 8)
}

func checkinActivationFixtureWithMinor(t *testing.T, private bool, minor int) (*Manager, Installed, string, string, string) {
	t.Helper()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-o", filepath.Join(directory, entry), "./testdata/checkinworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build check-in owner fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "checkin.fixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor},
			Capabilities: []sdk.Capability{sdk.ModuleRPC, sdk.OutboundExecutor, sdk.AccountAuth}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []codec.Protocol{"chat"},
			Checkin: &sdk.AccountCheckinDescriptor{Action: "daily.claim"}}},
		Actions:    []sdk.ActionDescriptor{{ID: "daily.claim", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Activation: []string{sdk.ActivationGateway},
	}
	if private {
		manifest.Actions[0].Component = "helper"
		manifest.Services = []sdk.ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: entry,
			Args: []string{"--checkin-helper"}, Contract: sdk.ServiceContract{ID: "checkin.helper", Version: 1},
			Methods: []string{"daily.claim"}, Start: "lazy"}}
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	installed, err := manager.Install(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "events")
	started := filepath.Join(t.TempDir(), "held")
	release := filepath.Join(t.TempDir(), "release")
	mode := ""
	if private {
		mode = "hold-service"
	}
	raw, _ = json.Marshal(map[string]any{"trace": trace, "mode": mode, "started": started, "release": release})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().SaveAccount(Account{Plugin: manifest.ID, Provider: "vendor", ID: "first",
		Auth: json.RawMessage(`{"token":"first"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	return manager, installed, trace, started, release
}

func waitCheckinFile(t *testing.T, path string, contains string) string {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), contains) {
			return string(raw)
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("owned lifecycle did not produce %q in %s", contains, path)
		}
	}
}

func TestGatewayActivationCloseRetiresCachedWorker(t *testing.T) {
	manager, _, trace, _, _ := checkinActivationFixture(t, false)
	owner, err := manager.BeginGatewayActivation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	waitCheckinFile(t, trace, `"event":"start"`)
	for deadline := time.Now().Add(15 * time.Second); !manager.Running("checkin.fixture"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("activation did not finish startup")
		}
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := waitCheckinFile(t, trace, `"event":"stop"`)
	if manager.isClosed() || manager.Running("checkin.fixture") || strings.Count(events, `"event":"start"`) != 1 ||
		strings.Count(events, `"event":"stop"`) != 1 {
		t.Fatalf("activation release retained its cached runtime: %s", events)
	}
}

func TestCheckinLeaseRechecksAfterPrivateStartup(t *testing.T) {
	for _, change := range []string{"account", "configuration"} {
		t.Run(change, func(t *testing.T) {
			manager, installed, trace, started, release := checkinActivationFixture(t, true)
			scope := sdk.Scope{Plugin: installed.Manifest.ID, Provider: "vendor", Account: "first", Generation: 1}
			config, err := manager.ModuleConfig(t.Context(), installed.Manifest.ID)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				execution CheckinExecution
				err       error
			}
			done := make(chan result, 1)
			go func() {
				execution, err := manager.CheckinAccount(t.Context(), scope, installed.Revision, config.Version,
					sdk.AccountCheckinRequest{Day: "2026-11-14"})
				done <- result{execution, err}
			}()
			t.Cleanup(func() {
				os.WriteFile(release, []byte("released"), 0600)
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Error("private check-in fixture did not stop")
				}
			})
			waitCheckinFile(t, started, "held")
			if change == "account" {
				account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, config.Version, config.Values); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(release, []byte("released"), 0600); err != nil {
				t.Fatal(err)
			}
			var completed result
			select {
			case completed = <-done:
				done <- completed
			case <-time.After(15 * time.Second):
				t.Fatal("private check-in did not finish")
			}
			raw, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			if completed.err == nil || strings.Contains(string(raw), `"event":"claim"`) ||
				completed.execution.Receipt.State != "not-started" {
				t.Fatalf("replaced %s dispatched after lazy startup: result=%+v err=%v trace=%s",
					change, completed.execution, completed.err, raw)
			}
		})
	}
}

func TestCheckinReservationWaitRespectsCancellation(t *testing.T) {
	manager, installed, trace, _, _ := checkinActivationFixture(t, false)
	scope := sdk.Scope{Plugin: installed.Manifest.ID, Provider: "vendor", Account: "first", Generation: 1}
	config, err := manager.ModuleConfig(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	unlock := sync.OnceFunc(func() { filelock.Unlock(lock) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admitted := make(chan struct{})
	completed := make(chan struct{})
	request := sdk.AccountCheckinRequest{Day: "2026-11-14"}
	var execution CheckinExecution
	var checkinErr error
	admissions := 0
	go func() {
		defer close(completed)
		execution, checkinErr = manager.CheckinAccountWithAdmission(ctx, scope, installed.Revision, config.Version, request,
			func(context.Context) error {
				admissions++
				if admissions != 2 {
					return nil
				}
				held, err := filelock.TryLock(lock)
				if err != nil || !held {
					return errors.Join(errors.New("fixture could not hold the private Store lock"), err)
				}
				close(admitted)
				return nil
			})
	}()
	defer func() {
		cancel()
		unlock()
		select {
		case <-completed:
		case <-time.After(15 * time.Second):
			t.Error("check-in fixture did not stop after lock release")
		}
	}()
	select {
	case <-admitted:
	case <-completed:
		t.Fatalf("check-in finished before reservation lock: %+v, %v", execution, checkinErr)
	case <-time.After(15 * time.Second):
		t.Fatal("check-in did not reach reservation lock")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Error("canceled check-in continued waiting to reserve its operation")
	}
	unlock()
	select {
	case <-completed:
	case <-time.After(15 * time.Second):
		t.Fatal("check-in did not return after lock release")
	}
	if !errors.Is(checkinErr, context.Canceled) || execution.Receipt.Scope.Operation != "" {
		t.Errorf("canceled reservation returned %+v, %v", execution, checkinErr)
	}
	intentPath := filepath.Join(manager.Namespace(), "private", scope.Plugin, "checkins", checkinIntentName(scope))
	if _, err := os.Lstat(intentPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canceled check-in left a reservation: %v", err)
	}
	events, err := os.ReadFile(trace)
	if err != nil || strings.Contains(string(events), `"event":"claim"`) {
		t.Fatalf("canceled check-in trace: %s, %v", events, err)
	}
	retry, err := manager.CheckinAccount(t.Context(), scope, installed.Revision, config.Version, request)
	if err != nil || retry.Receipt.State != "completed" || retry.Result == nil || retry.Result.Outcome != "claimed" {
		t.Fatalf("fresh check-in after canceled reservation: %+v, %v", retry, err)
	}
}

func TestCheckinRateLimitWaitsAcrossRestart(t *testing.T) {
	manager, installed, trace, _, _ := checkinActivationFixtureWithMinor(t, false, sdk.FullContract().Version.Minor)
	scope := sdk.Scope{Plugin: installed.Manifest.ID, Provider: "vendor", Account: "first", Generation: 1}
	config, err := manager.ModuleConfig(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	boundary := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil {
		t.Fatal(err)
	}
	account.Auth, _ = json.Marshal(map[string]string{"token": scope.Account, "mode": "rate-limited", "until": boundary})
	account, err = manager.Store().SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope.Generation = account.Generation
	request := sdk.AccountCheckinRequest{Day: "2026-11-22"}
	first, err := manager.CheckinAccount(t.Context(), scope, installed.Revision, config.Version, request)
	if err != nil || first.Result == nil || first.Result.Outcome != "rate-limited" || first.Result.Until != boundary ||
		first.Receipt.State != "completed" || first.Receipt.Scope.Operation == "" {
		t.Fatalf("completed rate-limit pause was rejected by ordinary check-in: result=%+v receipt=%+v error=%v",
			first.Result, first.Receipt, err)
	}
	journal, err := manager.Store().Operation(scope.Plugin, first.Receipt.Scope.Operation)
	if err != nil || journal.Scope != first.Receipt.Scope || journal.State != "completed" {
		t.Fatalf("rate-limit completion lacks a durable receipt: %+v %v", journal, err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(manager.root)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := restarted.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	account.Auth = json.RawMessage(`{"token":"first","mode":"done"}`)
	account, err = restarted.Store().SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope.Generation = account.Generation
	request.Day = "2026-11-23"
	held, err := restarted.CheckinAccount(t.Context(), scope, installed.Revision, config.Version, request)
	if err != nil || held.Result == nil || held.Result.Outcome != first.Result.Outcome ||
		held.Result.Until != boundary || held.Receipt != first.Receipt {
		t.Fatalf("replacement credential or owner day lost the pause: result=%+v receipt=%+v error=%v",
			held.Result, held.Receipt, err)
	}
	direct := scope
	direct.Operation, direct.Attempt = "direct-checkin", 1
	input, _ := json.Marshal(request)
	_, err = restarted.Call(t.Context(), scope.Plugin, sdk.MethodModuleCall, direct,
		sdk.ModuleCallRequest{Action: "daily.claim", Input: input}, nil)
	var refused *OutcomeError
	if !errors.As(err, &refused) || refused.Outcome != "not-started" {
		t.Fatalf("ordinary module.call bypassed the check-in intent: %v", err)
	}
	intentPath := filepath.Join(restarted.Namespace(), "private", scope.Plugin, "checkins", checkinIntentName(scope))
	var saved checkinIntent
	raw, err := os.ReadFile(intentPath)
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Result == nil || saved.Result.Until != boundary {
		t.Fatalf("read persisted pause: %s, %v", raw, err)
	}
	saved.Result.Until = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(intentPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.CheckinAccount(t.Context(), scope, installed.Revision, config.Version, request)
	if err != nil || next.Result == nil || next.Result.Outcome != "done" || next.Receipt.State != "completed" ||
		next.Receipt.Scope.Generation != scope.Generation || next.Receipt.Scope.Operation == first.Receipt.Scope.Operation {
		t.Fatalf("expired pause did not permit a fresh operation: result=%+v receipt=%+v error=%v", next.Result, next.Receipt, err)
	}
	events, err := os.ReadFile(trace)
	if err != nil || strings.Count(string(events), `"event":"claim"`) != 2 || strings.Contains(string(events), `"operation":"direct-checkin"`) {
		t.Fatalf("wanted one vendor invocation on each side of the pause: %s %v", events, err)
	}
}
