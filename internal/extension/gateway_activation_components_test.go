package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const activationHelper = "helper"
const activationStartupFixtureID = "activation-startup-fixture"

func holdActivationPrimaryStart(ctx context.Context, value sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
	var config struct {
		Gate string `json:"gate"`
	}
	if err := json.Unmarshal(value.Config.Values, &config); err != nil || config.Gate == "" {
		return sdk.ModuleReady{}, nil, err
	}
	if value.Services[activationHelper].Instance == "" {
		return sdk.ModuleReady{}, nil, errors.New("activation primary did not receive its eager helper")
	}
	if err := os.WriteFile(config.Gate+".entered", nil, 0o600); err != nil {
		return sdk.ModuleReady{}, nil, err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(config.Gate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			return sdk.ModuleReady{}, nil, err
		}
		select {
		case <-ctx.Done():
			return sdk.ModuleReady{}, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func serveActivationHelper() error {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		return err
	}
	var root sdk.Manifest
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	manifest, err := sdk.ServiceManifest(root, activationHelper)
	if err != nil {
		return err
	}
	return sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Module{Manifest: manifest})
}

func installActivationComponentFixture(t *testing.T, manager *Manager, id string) {
	t.Helper()
	source := activationFixturePackage(t)
	raw, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.ID = id
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ModuleRPC)
	manifest.Services = []sdk.ServiceDescriptor{{
		ID: activationHelper, Runtime: "exec", Entry: manifest.Entry, Args: []string{"--sdk-activation-helper"},
		Contract: sdk.ServiceContract{ID: "activation-fixture", Version: 1}, Start: "eager",
	}}
	catalogTestWriteManifest(t, source, manifest)
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
}

func waitActivationComponent(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("activation did not reach %s", what)
	}
}

func holdActivationHelperClose(t *testing.T, group *componentGroup) (*workerClient, <-chan struct{}, func()) {
	t.Helper()
	group.mu.Lock()
	defer group.mu.Unlock()
	component := group.components[activationHelper]
	if group.closed || component == nil || component.client == nil || component.client.processExit == nil {
		t.Fatal("activation group has no live exec helper")
	}
	helper := component.client
	stopping, released := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	stop := group.stop
	group.stop = func(client *workerClient) {
		if client == helper {
			close(stopping)
			<-released
		}
		stop(client)
	}
	return helper, stopping, release
}

func assertActivationHelperStillRunning(t *testing.T, primary, helper *workerClient) {
	t.Helper()
	select {
	case <-primary.done:
	default:
		t.Fatal("primary has not joined before the helper's stop barrier")
	}
	select {
	case <-helper.processExit:
		t.Fatal("helper exited before its transport was stopped")
	default:
	}
}

func TestGatewayActivationListenerJoinsRetiredHelpers(t *testing.T) {
	const id = "activation-components"
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	installActivationComponentFixture(t, manager, id)
	listener, stop := context.WithCancel(t.Context())
	defer stop()
	owner, err := manager.BeginGatewayActivation(listener)
	if err != nil {
		t.Fatal(err)
	}
	held, err := manager.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	waitActivation(t, "listener retain", func() bool {
		held.worker.mu.Lock()
		defer held.worker.mu.Unlock()
		return held.worker.active == 2
	})
	helper, stopping, release := holdActivationHelperClose(t, held.client.group)
	held.Close()
	stop()
	waitActivationComponent(t, "the helper's stop barrier", stopping)
	assertActivationHelperStillRunning(t, held.client, helper)

	wait, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := owner.Close(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listener released before its helper exited: %v", err)
	}
	var draining *ListenerOwnerDraining
	if _, err := manager.BeginGatewayActivation(t.Context()); !errors.As(err, &draining) {
		t.Fatalf("successor did not see the draining listener owner: %v", err)
	}

	release()
	joined, cancelJoin := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelJoin()
	if err := draining.Wait(joined); err != nil {
		t.Fatal(err)
	}
	select {
	case <-helper.processExit:
	default:
		t.Fatal("listener handoff preceded the original helper process exit")
	}
	successor, err := manager.BeginGatewayActivation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "successor's gateway activation", func() bool { return manager.Running(id) })
	if err := successor.Close(joined); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayActivationRestartsOnlyAfterRetiredHelpers(t *testing.T) {
	const retired, other = "activation-components", "activation-independent"
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	installActivationComponentFixture(t, manager, retired)
	installActivationFixture(t, manager, other, "")
	peer := NewCatalog(manager.Namespace())
	if err := peer.SetEnabled(t.Context(), other, false); err != nil {
		t.Fatal(err)
	}
	owner := activationTestOwner(t, manager)
	watch := newActivationWatch(manager.root)
	defer watch.close()
	reconcileActivationTest(t, owner, watch)
	lease := owner.leases[retired].Lease
	if lease == nil {
		t.Fatal("original gateway group did not activate")
	}
	helper, stopping, release := holdActivationHelperClose(t, lease.client.group)

	if err := peer.SetEnabled(t.Context(), retired, false); err != nil {
		t.Fatal(err)
	}
	owner.reconcile(watch)
	waitActivationComponent(t, "the helper's stop barrier", stopping)
	assertActivationHelperStillRunning(t, lease.client, helper)
	if err := peer.SetEnabled(t.Context(), retired, true); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetEnabled(t.Context(), other, true); err != nil {
		t.Fatal(err)
	}
	owner.reconcile(watch)
	if _, starting := owner.starting[retired]; starting || manager.Running(retired) {
		t.Fatal("same-module replacement started before its original helper exited")
	}
	reconcileActivationTest(t, owner, watch)
	if !manager.Running(other) {
		t.Fatal("independent module did not activate while the original helper drained")
	}
	assertActivationHelperStillRunning(t, lease.client, helper)

	release()
	waitActivationComponent(t, "original group retirement", lease.client.group.closeDone)
	reconcileActivationTest(t, owner, watch)
	if next := owner.leases[retired].Lease; next == nil || next.client == lease.client {
		t.Fatal("same-module replacement did not activate after the original group retired")
	}
}

func TestGatewayActivationFencesOrdinaryAcquisitionDuringHelperRetirement(t *testing.T) {
	for _, test := range []struct {
		name         string
		requestFirst bool
		closeManager bool
		lateStart    bool
		sameConfig   bool
		shutdown     bool
		restart      bool
	}{
		{name: "reconciler retires changed config"},
		{name: "request retires new activation", requestFirst: true},
		{name: "late activation result retires changed config", lateStart: true},
		{name: "late withdrawal allows same config", lateStart: true, sameConfig: true},
		{name: "listener shutdown then peer config change", shutdown: true},
		{name: "same config restart keeps earlier drain", restart: true},
		{name: "manager closes while helper drains", closeManager: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const id, independent = "activation-components", "activation-independent"
			manager := NewManager(t.TempDir())
			closeActivationTestManager(t, manager)
			installActivationComponentFixture(t, manager, id)
			installActivationFixture(t, manager, independent, "")
			peer := NewManager(manager.Namespace())
			closeActivationTestManager(t, peer)
			ctx := t.Context()
			if err := peer.catalog.SetEnabled(ctx, independent, false); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(t.TempDir(), "trace.jsonl")
			setTrace := func(host *Manager, version, filename string) sdk.ConfigSnapshot {
				t.Helper()
				config, _ := json.Marshal(map[string]string{"trace": filename})
				current, err := host.CompareAndSwapModuleConfig(ctx, id, version, config)
				if err != nil {
					t.Fatal(err)
				}
				return current
			}
			originalConfig := setTrace(manager, "0", trace)
			owner := newGatewayActivation(manager, ctx)
			shutdown := sync.OnceFunc(owner.shutdown)
			t.Cleanup(shutdown)
			watch := newActivationWatch(manager.Namespace())
			defer watch.close()
			var pending activationResult
			var original *Lease
			if test.lateStart {
				owner.reconcile(watch)
				wait, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				select {
				case pending = <-owner.started:
					defer func() {
						if pending.id != "" {
							owner.finishStart(pending)
						}
					}()
				case <-wait.Done():
					t.Fatal("original group did not finish startup")
				}
				original = pending.lease
			} else {
				reconcileActivationTest(t, owner, watch)
				original = owner.leases[id].Lease
			}
			if original == nil {
				t.Fatal("original gateway group did not activate")
			}
			info := sdk.GatewayContext{Origin: "local", Protocol: "chat", Model: "fixture", Session: "session"}
			callGateway := func(ctx context.Context) bool {
				run := manager.BeginGateway(ctx, []byte(`{"model":"fixture","messages":[]}`), info)
				if run == nil {
					return false
				}
				run.Close()
				return true
			}
			if !callGateway(ctx) || len(gatewayTrace(t, trace)) != 1 {
				t.Fatal("original helper fixture did not serve a gateway hook")
			}
			helper, stopping, release := holdActivationHelperClose(t, original.client.group)
			wantTrace, wantVersion := trace, originalConfig.Version
			if test.shutdown {
				go shutdown()
				waitActivationComponent(t, "the shutting down listener's helper", stopping)
			}
			if test.restart {
				stopActivationWorker(t, original.client)
				restarted, err := owner.start(ctx, moduleGeneration{original.worker.entry, originalConfig})
				if err != nil {
					t.Fatal(err)
				}
				if restarted.client == original.client {
					restarted.Close()
					t.Fatal("same generation did not replace its failed worker")
				}
				replaced := owner.watchWorker(restarted)
				original.Close()
				owner.withdraw(replaced)
				waitActivationComponent(t, "the same-config replacement's component closure", replaced.retired)
				assertActivationHelperStillRunning(t, original.client, helper)
			}
			if test.requestFirst {
				for _, enabled := range []bool{false, true} {
					if err := peer.catalog.SetEnabled(ctx, id, enabled); err != nil {
						t.Fatal(err)
					}
				}
				if callGateway(ctx) || len(gatewayTrace(t, trace)) != 1 {
					t.Error("gateway dispatched the re-enabled module before the old helper retired")
				}
			} else if !test.sameConfig {
				wantTrace = filepath.Join(t.TempDir(), "changed.jsonl")
				wantVersion = setTrace(peer, originalConfig.Version, wantTrace).Version
			}
			if test.lateStart {
				if test.sameConfig {
					owner.cancel()
				} else {
					pending.err = original.CheckCurrent(ctx)
					if pending.err == nil {
						t.Fatal("late startup incorrectly accepted replaced configuration")
					}
				}
				owner.finishStart(pending)
				pending = activationResult{}
			} else if !test.shutdown && !test.restart {
				owner.reconcile(watch)
			}
			waitActivationComponent(t, "the helper's stop barrier", stopping)
			assertActivationHelperStillRunning(t, original.client, helper)

			if test.sameConfig {
				if !callGateway(ctx) || len(gatewayTrace(t, trace)) != 2 {
					t.Fatal("same-generation gateway replacement could not start during listener withdrawal")
				}
				return
			}
			if !test.closeManager {
				if acquired, err := manager.AcquireRevision(ctx, id, original.Revision(), wantVersion); err == nil {
					acquired.Close()
					t.Error("ordinary acquisition started a replacement before the old helper retired")
				}
				if callGateway(ctx) {
					t.Error("gateway executed a replacement hook while the old helper drained")
				}
			}
			if err := peer.catalog.SetEnabled(ctx, independent, true); err != nil {
				t.Fatal(err)
			}
			if test.shutdown {
				// The HTTP route can still use the manager during activation owner handoff.
				independentLease, err := manager.Acquire(ctx, independent)
				if err != nil {
					t.Fatal(err)
				}
				independentLease.Close()
			} else {
				reconcileActivationTest(t, owner, watch)
			}
			if !manager.Running(independent) {
				t.Fatal("unrelated module could not activate while the old helper drained")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if acquired, err := manager.AcquireRevision(canceled, id, original.Revision(), wantVersion); !errors.Is(err, context.Canceled) {
				if acquired != nil {
					acquired.Close()
				}
				t.Errorf("canceled acquire while helper drained: %v", err)
			}
			if test.closeManager {
				if err := manager.Close(canceled); !errors.Is(err, context.Canceled) {
					t.Fatalf("manager did not leave its helper to drain: %v", err)
				}
				if callGateway(ctx) {
					t.Fatal("closed manager admitted a replacement gateway hook")
				}
			}
			release()
			waitActivationComponent(t, "old component group closure", original.client.group.closeDone)
			waitActivationComponent(t, "old helper process exit", helper.processExit)
			if test.closeManager {
				if err := manager.Close(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
			before := len(gatewayTrace(t, wantTrace))
			if !callGateway(ctx) || len(gatewayTrace(t, wantTrace)) != before+1 {
				t.Fatal("ordinary gateway did not use the new generation after helper retirement")
			}
		})
	}
}

func TestGatewayActivationFencesOrdinaryAcquisitionDuringHelperStartup(t *testing.T) {
	ctx := t.Context()
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	peer := NewManager(manager.Namespace())
	closeActivationTestManager(t, peer)
	installActivationComponentFixture(t, manager, activationStartupFixtureID)
	owner := activationTestOwner(t, manager)
	watch := newActivationWatch(manager.Namespace())
	defer watch.close()
	state := t.TempDir()
	gate, trace := filepath.Join(state, "start"), filepath.Join(state, "replacement.jsonl")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(gate)
	oldConfig, err := json.Marshal(map[string]string{"gate": gate})
	if err != nil {
		t.Fatal(err)
	}
	old, err := manager.CompareAndSwapModuleConfig(ctx, activationStartupFixtureID, "0", oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	owner.reconcile(watch)
	waitActivation(t, "original primary configuring with its eager helper", func() bool {
		_, err := os.Stat(gate + ".entered")
		return err == nil
	})
	newConfig, err := json.Marshal(map[string]string{"trace": trace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = peer.CompareAndSwapModuleConfig(ctx, activationStartupFixtureID, old.Version, newConfig); err != nil {
		t.Fatal(err)
	}
	callGateway := func() bool {
		body := []byte(`{"model":"fixture","messages":[]}`)
		info := sdk.GatewayContext{Origin: "local", Protocol: "chat", Model: "fixture", Session: "session"}
		run := manager.BeginGateway(ctx, body, info)
		if run == nil {
			return false
		}
		run.Close()
		return true
	}
	if callGateway() || len(gatewayTrace(t, trace)) != 0 {
		t.Error("gateway executed a replacement hook during the old helper group's startup")
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	join, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case result := <-owner.started:
		owner.finishStart(result)
		if result.id != activationStartupFixtureID || result.lease != nil || result.err == nil {
			t.Fatalf("old startup was not rejected after config changed: %#v", result)
		}
	case <-join.Done():
		t.Fatal("old startup did not join after its gate was released")
	}
	before := len(gatewayTrace(t, trace))
	if !callGateway() || len(gatewayTrace(t, trace)) != before+1 {
		t.Fatal("gateway did not use the new generation after the old startup failed")
	}
}
