package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func checkinActivationFixture(t *testing.T, private bool) (*Manager, Installed, string, string, string) {
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
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
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
