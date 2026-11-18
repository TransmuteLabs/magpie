package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionDefaultsRig struct {
	*laneRig
	vendor    *slowVendor
	mu        sync.Mutex
	accounts  []string
	id        string
	manager   *extensionhost.Manager
	manifest  sdk.Manifest
	directory string
}

func newExtensionDefaultsRig(t *testing.T, concurrency, accounts int) *extensionDefaultsRig {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_ADDR", "")
	rig := &extensionDefaultsRig{vendor: &slowVendor{release: map[string]chan struct{}{}, quit: make(chan struct{})}}
	vendor := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rig.mu.Lock()
		rig.accounts = append(rig.accounts, request.Header.Get("X-Fixture-Account"))
		rig.mu.Unlock()
		rig.vendor.ServeHTTP(writer, request)
	}))
	t.Cleanup(vendor.Close)
	t.Cleanup(func() { close(rig.vendor.quit) })
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	buildContext, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	build := proc.CommandContext(buildContext, filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/defaultsworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build defaults fixture: %v\n%s", err, output)
	}
	manifest := extensionGatewayManifest()
	manifest.ID, manifest.Entry = "fixture.defaults", entry
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.AccountAuth)
	defaults, _ := json.Marshal(map[string]int{"maxConcurrency": concurrency, "maxAccounts": accounts})
	if err := json.Unmarshal(defaults, &manifest.Providers[0]); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"alice", "bob", "charlie"} {
		auth, _ := json.Marshal(map[string]string{"upstream": vendor.URL})
		if _, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: manifest.ID,
			Provider: manifest.Providers[0].ID, ID: account, Auth: auth}, 0); err != nil {
			t.Fatal(err)
		}
	}
	provider.Changed()
	rig.id = provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
	rig.manager, rig.manifest = manager, manifest
	rig.directory = directory
	if err := provider.SetAccountOrder(rig.id, []string{"alice", "bob", "charlie"}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	gateway := httptest.NewServer(server.Front())
	t.Cleanup(gateway.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	rig.laneRig = &laneRig{t: t, s: server, gw: gateway, replies: make(chan laneReply, 20), ctx: ctx}
	return rig
}

func (rig *extensionDefaultsRig) seenAccounts() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return slices.Clone(rig.accounts)
}

func (rig *extensionDefaultsRig) waiting() int {
	count := 0
	for _, lane := range rig.s.Lanes() {
		count += lane.Waiting
	}
	return count
}

func (rig *extensionDefaultsRig) pauseOthers(t *testing.T) {
	t.Helper()
	for _, user := range []string{"bob", "charlie"} {
		if err := provider.SetLoginOn(rig.id, user, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtensionDefaultsFrontHoldsAndCancellation(t *testing.T) {
	rig := newExtensionDefaultsRig(t, 3, 2)
	rig.pauseOthers(t)
	for _, tag := range []string{"a", "b", "c"} {
		rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", tag)
		within(t, tag+" at vendor", func() bool { return slices.Contains(rig.vendor.seen(), tag) })
	}
	ctx, cancel := context.WithCancel(t.Context())
	rig.send(ctx, "/v1/chat/completions", rig.id+"/m1", "cancel")
	within(t, "fourth request held by SDK default", func() bool {
		return rig.waiting() == 1 || slices.Contains(rig.vendor.seen(), "cancel")
	})
	if rig.waiting() != 1 {
		t.Fatal("fourth ordinary Front request bypassed SDK per-account default")
	}
	cancel()
	if reply := rig.reply(); reply.tag != "cancel" || reply.err == nil {
		t.Fatalf("queued cancellation: %+v", reply)
	}
	within(t, "canceled request removed", func() bool { return rig.waiting() == 0 })
	rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", "after")
	within(t, "next request queued", func() bool { return rig.waiting() == 1 })
	close(rig.vendor.gate("a"))
	within(t, "EOF releases slot", func() bool { return slices.Contains(rig.vendor.seen(), "after") })
	for _, tag := range []string{"b", "c", "after"} {
		close(rig.vendor.gate(tag))
	}
	for range 4 {
		if reply := rig.reply(); reply.err != nil || reply.status != 200 {
			t.Fatalf("held reply: %+v", reply)
		}
	}
	within(t, "all SDK lane slots released", func() bool { return len(rig.s.Lanes()) == 0 })
	if slices.Contains(rig.vendor.seen(), "cancel") || rig.vendor.most != 3 {
		t.Fatalf("vendor got canceled/extra work: seen=%v peak=%d", rig.vendor.seen(), rig.vendor.most)
	}
}

func TestExtensionDefaultsFrontFanoutAndExplicitUserZero(t *testing.T) {
	rig := newExtensionDefaultsRig(t, 1, 2)
	for _, tag := range []string{"a", "b"} {
		rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", tag)
		within(t, tag+" at vendor", func() bool { return slices.Contains(rig.vendor.seen(), tag) })
	}
	rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", "c")
	within(t, "third request queued or wrongly dispatched", func() bool {
		return rig.waiting() == 1 || slices.Contains(rig.vendor.seen(), "c")
	})
	if rig.waiting() != 1 || slices.Contains(rig.seenAccounts(), "charlie") {
		t.Fatalf("SDK account fanout bypassed: accounts=%v waiting=%d", rig.seenAccounts(), rig.waiting())
	}
	if err := provider.SetAtOnce(rig.id, new(int)); err != nil {
		t.Fatal(err)
	}
	rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", "user")
	within(t, "explicit user zero releases account reserve", func() bool { return slices.Contains(rig.seenAccounts(), "charlie") })
	for _, tag := range []string{"a", "b", "user"} {
		close(rig.vendor.gate(tag))
	}
	within(t, "queued request dispatches", func() bool { return slices.Contains(rig.vendor.seen(), "c") })
	close(rig.vendor.gate("c"))
	for range 4 {
		if reply := rig.reply(); reply.err != nil || reply.status != 200 {
			t.Fatalf("fanout reply: %+v", reply)
		}
	}
	rig.pauseOthers(t)
	current, err := provider.Find(rig.id)
	if err != nil {
		t.Fatal(err)
	}
	current.MaxConcurrency = new(int)
	if err := provider.Save(*current); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"zero1", "zero2"} {
		rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", tag)
		within(t, tag+" dispatched despite default one", func() bool { return slices.Contains(rig.vendor.seen(), tag) })
	}
	for _, tag := range []string{"zero1", "zero2"} {
		close(rig.vendor.gate(tag))
		if reply := rig.reply(); reply.err != nil || reply.status != 200 {
			t.Fatalf("user zero reply: %+v", reply)
		}
	}
	within(t, "all fanout slots returned", func() bool { return len(rig.s.Lanes()) == 0 })
}

func TestExtensionDefaultsFrontQueuedSelectionRevoked(t *testing.T) {
	for _, change := range []string{"off", "config", "account", "package"} {
		t.Run(change, func(t *testing.T) {
			rig := newExtensionDefaultsRig(t, 1, 2)
			rig.pauseOthers(t)
			rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", "held")
			within(t, "held request at vendor", func() bool { return slices.Contains(rig.vendor.seen(), "held") })
			rig.send(t.Context(), "/v1/chat/completions", rig.id+"/m1", "stale")
			within(t, "request waiting before selection changes", func() bool { return rig.waiting() == 1 })
			switch change {
			case "off":
				if err := rig.manager.SetEnabled(t.Context(), rig.manifest.ID, false); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := rig.manager.CompareAndSwapModuleConfig(t.Context(), rig.manifest.ID, "0",
					json.RawMessage(`{"revision":"changed"}`)); err != nil {
					t.Fatal(err)
				}
			case "account":
				account, err := rig.manager.Store().GetAccount(rig.manifest.ID, rig.manifest.Providers[0].ID, "alice")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := rig.manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "package":
				manifest := rig.manifest.Clone()
				manifest.Version = "1.0.1"
				if err := json.Unmarshal([]byte(`{"maxConcurrency":2}`), &manifest.Providers[0]); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(manifest)
				if err := os.WriteFile(filepath.Join(rig.directory, sdk.ManifestFile), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := rig.manager.Install(t.Context(), rig.directory); err != nil {
					t.Fatal(err)
				}
			}
			close(rig.vendor.gate("held"))
			for range 2 {
				reply := rig.reply()
				if reply.tag == "stale" && reply.status == 200 {
					t.Fatalf("queued stale %s selection executed: %+v", change, reply)
				}
			}
			if slices.Contains(rig.vendor.seen(), "stale") {
				t.Fatalf("stale %s selection reached vendor", change)
			}
			within(t, "revoked selection releases every slot", func() bool { return len(rig.s.Lanes()) == 0 })
		})
	}
}
