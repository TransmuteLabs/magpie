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
