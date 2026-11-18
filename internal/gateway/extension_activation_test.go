package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func activationFixture(t *testing.T) (*extensionhost.Manager, string, string) {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := extensionhost.Default()
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o",
		filepath.Join(directory, entry), "../extension/testdata/checkinworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build activation fixture: %v\n%s", err, output)
	}
	raw, _ := json.Marshal(map[string]any{
		"schema": 1, "id": "activation.fixture", "version": "1.0.0", "runtime": "exec", "entry": entry,
		"contract":   map[string]any{"version": map[string]int{"major": 1, "minor": 8}, "capabilities": []string{"module-rpc"}},
		"actions":    []any{map[string]any{"id": "daily.claim", "effect": "write", "inputSchema": map[string]any{"type": "object"}}},
		"activation": []string{"gateway"},
	})
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "events.jsonl")
	raw, _ = json.Marshal(map[string]any{"trace": trace})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "activation.fixture", "0", raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager, trace, directory
}

func activationEvents(t *testing.T, trace string, count int) string {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var raw []byte
	for {
		var err error
		raw, err = os.ReadFile(trace)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Count(strings.TrimSpace(string(raw)), "\n")+boolInt(len(raw) > 0) >= count {
			return string(raw)
		}
		select {
		case <-deadline.C:
			t.Fatalf("gateway activation lifecycle missing events: want %d, got %s", count, raw)
		case <-tick.C:
		}
	}
}

func TestExtensionGatewayActivationFollowsActualListener(t *testing.T) {
	manager, trace, directory := activationFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	web := httptest.NewServer(server.Front())
	response, err := web.Client().Get(web.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	web.Close()
	if manager.Running("activation.fixture") {
		t.Fatal("New/Front started a gateway activation")
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_ADDR", busy.Addr().String())
	if err := server.ListenAndServe(t.Context()); err == nil || manager.Running("activation.fixture") {
		busy.Close()
		t.Fatalf("failed bind activated a worker: %v", err)
	}
	busy.Close()
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- server.ListenAndServe(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("ordinary gateway shutdown did not join activation")
		}
	})
	activationEvents(t, trace, 1)
	waitFor(t, "bound gateway activation ready", func() bool { return manager.Running("activation.fixture") })
	config, err := manager.ModuleConfig(t.Context(), "activation.fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "activation.fixture", config.Version, config.Values); err != nil {
		t.Fatal(err)
	}
	events := activationEvents(t, trace, 3)
	lines := strings.Split(strings.TrimSpace(events), "\n")
	if !strings.Contains(lines[0], `"event":"start"`) || !strings.Contains(lines[1], `"event":"stop"`) ||
		!strings.Contains(lines[2], `"event":"start"`) {
		t.Fatalf("configuration replacement did not withdraw before activation: %s", events)
	}
	external := extensionhost.NewCatalog(manager.Namespace())
	if err := external.SetEnabled(t.Context(), "activation.fixture", false); err != nil {
		t.Fatal(err)
	}
	activationEvents(t, trace, 4)
	if manager.Running("activation.fixture") {
		t.Fatal("disabled activation is still running")
	}
	if err := external.SetEnabled(t.Context(), "activation.fixture", true); err != nil {
		t.Fatal(err)
	}
	activationEvents(t, trace, 5)
	if err := manager.Remove(t.Context(), "activation.fixture"); err != nil {
		t.Fatal(err)
	}
	activationEvents(t, trace, 6)
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	activationEvents(t, trace, 7)
	waitFor(t, "reinstalled gateway activation ready", func() bool { return manager.Running("activation.fixture") })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway did not join active module shutdown")
	}
	activationEvents(t, trace, 8)
	if manager.Running("activation.fixture") {
		t.Fatal("gateway shutdown retained activated generation")
	}
}

func TestExtensionGatewayActivationFailureKeepsOrdinaryHTTP(t *testing.T) {
	manager, trace, _ := activationFixture(t)
	config, err := manager.ModuleConfig(t.Context(), "activation.fixture")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"trace": trace, "mode": "startup-error"})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "activation.fixture", config.Version, raw); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	t.Setenv("PATH", os.Getenv(standInsEnv))
	t.Setenv("MAGPIE_BUN", "")
	server := New()
	t.Cleanup(server.idle.stop)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- server.ListenAndServe(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("failed activation was not joined")
		}
	})
	activationEvents(t, trace, 2)
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+server.Addr()+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || manager.Running("activation.fixture") {
		t.Fatalf("activation failure stopped ordinary HTTP: status=%d worker=%v", response.StatusCode, manager.Running("activation.fixture"))
	}
}
