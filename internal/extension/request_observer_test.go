package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKObserversRejectRawWorkerResult(t *testing.T) {
	source := fixturePackage(t)
	manifest := sdk.Manifest{
		Schema: 1, ID: "raw-observer", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Args: []string{"--sdk-raw-observer"},
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 12},
			Capabilities: []sdk.Capability{sdk.LedgerObserver, sdk.RequestObserver}},
		Ledger: &sdk.LedgerObservationDescriptor{}, RequestObserver: &sdk.RequestObservationDescriptor{},
	}
	if filepath.Ext(os.Args[0]) == ".exe" {
		manifest.Entry += ".exe"
	}
	catalogTestWriteManifest(t, source, manifest)
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	for _, observer := range []struct {
		name    string
		list    func(*Manager, context.Context) ([]LedgerTarget, error)
		deliver func(LedgerTarget, string) error
	}{
		{"ledger", (*Manager).LedgerTargets, func(target LedgerTarget, value string) error {
			return manager.ObserveLedger(t.Context(), target, sdk.LedgerObservation{Time: 1, Model: value})
		}},
		{"request", (*Manager).RequestObservationTargets, func(target LedgerTarget, value string) error {
			return manager.ObserveRequest(t.Context(), target, sdk.RequestObservation{Phase: "start", RequestID: value, Time: 1})
		}},
	} {
		t.Run(observer.name, func(t *testing.T) {
			targets, err := observer.list(manager, t.Context())
			if err != nil || len(targets) != 1 {
				t.Fatalf("observer discovery got %+v: %v", targets, err)
			}
			for _, sample := range []string{"abc", "bad", "def"} {
				err := observer.deliver(targets[0], sample)
				if sample != "bad" {
					if err != nil {
						t.Fatalf("valid acknowledgement: %v", err)
					}
				} else {
					var fault *sdk.Error
					if !errors.As(err, &fault) || fault.Code != "invalid_result" {
						t.Errorf("nonempty acknowledgement must fail as invalid_result, got %v", err)
					}
				}
			}
		})
	}
}

func TestRequestObserverHostAdmissionDeniesAllCallbacks(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := sdk.Manifest{ID: "observer"}
	scope := sdk.Scope{Plugin: manifest.ID}
	ctx := context.WithValue(t.Context(), hostAuthorityKey{}, hostAuthority{
		manifest: manifest, method: sdk.MethodRequestObserve,
	})
	for _, method := range []string{sdk.MethodStateGet, sdk.MethodStateCAS, sdk.MethodUsageAppend, sdk.MethodComponentCall, sdk.MethodNetworkProxy} {
		_, err := manager.hostCall(ctx, scope, method, json.RawMessage(`{"key":"observer-probe","version":"0","value":true}`))
		if err == nil || !strings.Contains(err.Error(), "request observation cannot call host services") {
			t.Errorf("%s reached host service dispatch: %v", method, err)
		}
	}
	if err := validateCallScope(manifest, sdk.MethodRequestObserve, sdk.Scope{Plugin: manifest.ID, Session: "private"}); err == nil {
		t.Fatal("request observer borrowed session scope")
	}
}

func TestSDKObserverAdmissionIsolatesBrokenConfigAndRejectsPreviousEnablement(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	install := func(id string) {
		t.Helper()
		source := fixturePackage(t)
		manifest := sdk.Manifest{
			Schema: 1, ID: id, Version: "1.0.0", Runtime: "exec", Entry: "worker", Args: []string{"--sdk-test-worker"},
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 12},
				Capabilities: []sdk.Capability{sdk.LedgerObserver, sdk.RequestObserver}},
			Ledger: &sdk.LedgerObservationDescriptor{}, RequestObserver: &sdk.RequestObservationDescriptor{},
		}
		if filepath.Ext(os.Args[0]) == ".exe" {
			manifest.Entry += ".exe"
		}
		catalogTestWriteManifest(t, source, manifest)
		if _, err := manager.Install(t.Context(), source); err != nil {
			t.Fatal(err)
		}
	}
	const broken, healthy = "a-broken", "z-healthy"
	install(broken)
	install(healthy)
	trace := filepath.Join(t.TempDir(), "observer.jsonl")
	values, err := json.Marshal(map[string]string{"trace": trace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), healthy, "0", values); err != nil {
		t.Fatal(err)
	}
	brokenScope := sdk.Scope{Plugin: broken}
	if _, err := manager.Store().StateCAS(brokenScope, moduleConfigKey, 0, json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}

	if lease, err := manager.Acquire(t.Context(), broken); err == nil {
		lease.Close()
		t.Fatal("broken configuration did not fail closed")
	}
	for _, observer := range []struct {
		name    string
		list    func(*Manager, context.Context) ([]LedgerTarget, error)
		deliver func(*Manager, context.Context, LedgerTarget) error
	}{
		{"ledger", (*Manager).LedgerTargets, func(m *Manager, ctx context.Context, target LedgerTarget) error {
			return m.ObserveLedger(ctx, target, sdk.LedgerObservation{Time: 1, Model: "ledger-probe"})
		}},
		{"request", (*Manager).RequestObservationTargets, func(m *Manager, ctx context.Context, target LedgerTarget) error {
			return m.ObserveRequest(ctx, target, sdk.RequestObservation{Phase: "start", RequestID: "abc", Time: 1})
		}},
	} {
		t.Run(observer.name, func(t *testing.T) {
			wantTarget := func(m *Manager) LedgerTarget {
				t.Helper()
				targets, err := observer.list(m, t.Context())
				if err != nil || len(targets) != 1 || targets[0].Module != healthy {
					t.Fatalf("discovery must retain only the healthy observer: %v, %v", targets, err)
				}
				return targets[0]
			}
			if err := os.Remove(trace); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			before := wantTarget(manager)
			if err := observer.deliver(manager, t.Context(), before); err != nil {
				t.Fatalf("a corrupt neighbor prevented actual delivery: %v", err)
			}
			first, err := os.ReadFile(trace)
			if err != nil || bytes.Count(first, []byte("\n")) != 1 {
				t.Fatalf("healthy worker did not record exactly one observation: %s, %v", first, err)
			}
			// A different Manager changes catalog intent. The serving manager
			// never sees a disabled catalog snapshot between deliveries.
			other := NewManager(manager.root)
			if err := other.SetEnabled(t.Context(), healthy, false); err != nil {
				t.Fatal(err)
			}
			if err := other.SetEnabled(t.Context(), healthy, true); err != nil {
				t.Fatal(err)
			}
			if err := observer.deliver(manager, t.Context(), before); err == nil {
				t.Fatal("stale event was delivered after disable and re-enable")
			}
			still, err := os.ReadFile(trace)
			if err != nil || !bytes.Equal(first, still) {
				t.Fatal("stale event reached the worker trace", err)
			}
			after := wantTarget(manager)
			if after.Revision != before.Revision || after.Directory != before.Directory || after.ConfigVersion != before.ConfigVersion {
				t.Fatal("enablement changed package or configuration identity")
			}
			if after.EnableGeneration == before.EnableGeneration {
				t.Fatal("enablement identity was reused")
			}
			if err := other.SetEnabled(t.Context(), healthy, true); err != nil {
				t.Fatal(err)
			}
			if wantTarget(manager) != after {
				t.Fatal("enabling an already enabled module advanced the generation")
			}
			if err := observer.deliver(manager, t.Context(), after); err != nil {
				t.Fatalf("current enabled target could not receive events: %v", err)
			}
			final, err := os.ReadFile(trace)
			if err != nil || !bytes.Equal(final, append(bytes.Clone(first), first...)) {
				t.Fatalf("replacement generation did not write exactly one additional observation: %s, %v", final, err)
			}
		})
	}
	saved, err := manager.Store().StateGet(brokenScope, moduleConfigKey)
	if err != nil || string(saved.Value) != "[]" || saved.Version != 1 {
		t.Fatalf("unreadable module configuration was changed: %+v, %v", saved, err)
	}
}
