//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	hybrid "github.com/yetone/magpie/examples/extensions/hybrid-go"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var compositeDistributionV1 = flag.String("composite-test-v1", "", "parent-owned hybrid version 1 distribution")
var compositeDistributionV2 = flag.String("composite-test-v2", "", "parent-owned hybrid version 2 distribution")
var compositeRoot = flag.String("composite-test-root", "", "parent-owned hybrid test catalogs")

const compositeID = "sdk-hybrid-go"

func compositeBuild(t *testing.T, version string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "distribution")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, "python3", "examples/extensions/hybrid-go/build.py", destination, "--version", version)
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build public hybrid %s: %v\n%s", version, err, output)
	}
	body, err := os.ReadFile(filepath.Join(destination, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := sdk.CheckCompatibility(manifest); err != nil {
		t.Fatal(err)
	}
	return destination
}

func TestCompositeHybridBundle(t *testing.T) {
	v1, v2, root := *compositeDistributionV1, *compositeDistributionV2, *compositeRoot
	if v1 == "" {
		v1, v2, root = compositeBuild(t, "1.0.0"), compositeBuild(t, "2.0.0"), t.TempDir()
		if runtime.GOOS == "windows" {
			// Loaded DLLs stay mapped until process exit. Their parent owns cleanup.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestCompositeHybridBundle$", "-test.v",
				"-composite-test-v1="+v1, "-composite-test-v2="+v2, "-composite-test-root="+root)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("hybrid Windows host fixture: %v\n%s", err, output)
			} else {
				t.Log(string(output))
			}
			return
		}
	}
	if v2 == "" || root == "" {
		t.Fatal("hybrid fixture requires both distributions and parent-owned catalogs")
	}
	t.Run("scope-readiness-and-off-on", func(t *testing.T) {
		compositeScopesAndReadiness(t, v1, filepath.Join(root, "scopes"))
	})
	t.Run("native-stream-and-nested-cancel", func(t *testing.T) {
		compositeStreamAndCancel(t, v1, filepath.Join(root, "streams"))
	})
	t.Run("old-generation-lazy-helper", func(t *testing.T) {
		compositeFrozenGeneration(t, v1, v2, filepath.Join(root, "generations"))
	})
	t.Run("partial-startup-cleanup", func(t *testing.T) {
		compositePartialStartup(t, v1, filepath.Join(root, "partial"))
	})
}

func compositeManager(t *testing.T, distribution, root string) (*Manager, context.Context, Installed) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	manager := NewManager(root)
	manager.Bun = filepath.Join(root, "bun-does-not-exist")
	t.Cleanup(func() {
		cancel()
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	entry, err := manager.Install(ctx, distribution)
	if err != nil {
		t.Fatal(err)
	}
	return manager, ctx, entry
}

func compositeConfig(t *testing.T, manager *Manager, ctx context.Context, expected, values string) sdk.ConfigSnapshot {
	t.Helper()
	updated, err := manager.CompareAndSwapModuleConfig(ctx, compositeID, expected, json.RawMessage(values))
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func compositeAction(t *testing.T, manager *Manager, ctx context.Context, action string, scope sdk.Scope, input string, out any) {
	t.Helper()
	if err := manager.Invoke(ctx, compositeID, sdk.MethodModuleCall, scope,
		sdk.ModuleCallRequest{Action: action, Input: json.RawMessage(input)}, out, nil); err != nil {
		t.Fatalf("module action %s: %v (cause: %v)", action, err, errors.Unwrap(err))
	}
}

func compositeAccount(t *testing.T, manager *Manager, ctx context.Context, id string) (sdk.Scope, string) {
	t.Helper()
	scope := sdk.Scope{Plugin: compositeID, Provider: "hybrid-fixture"}
	input, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	var result sdk.AuthResult
	if err := manager.Invoke(ctx, compositeID, sdk.MethodAuthFinish, scope,
		sdk.AuthFinishRequest{Input: input}, &result, nil); err != nil {
		t.Fatal(err)
	}
	if result.ID != id || len(result.Credentials) == 0 {
		t.Fatalf("synthetic auth returned the wrong account: %s", result.ID)
	}
	account, err := manager.Store().SaveAccount(Account{
		Plugin: compositeID, Provider: scope.Provider, ID: id, Auth: result.Credentials,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope.Account, scope.Generation = account.ID, account.Generation
	digest := sha256.Sum256(account.Auth)
	return scope, hex.EncodeToString(digest[:])
}

func compositeCheckIdentity(t *testing.T, value hybrid.Identity, component, version, config, setting string) {
	t.Helper()
	if value.PID <= 0 || value.Instance == "" || value.Component != component || value.Version != version ||
		value.ConfigVersion != config || value.Value != setting {
		t.Fatalf("component identity/configuration mismatch: %+v", value)
	}
	if component == sdk.PrimaryComponent && value.PID != os.Getpid() ||
		component == "backend" && value.PID == os.Getpid() {
		t.Fatalf("wrong execution architecture: component=%s handlerPID=%d hostPID=%d", component, value.PID, os.Getpid())
	}
}

func compositeScopesAndReadiness(t *testing.T, distribution, root string) {
	manager, ctx, entry := compositeManager(t, distribution, root)
	config := compositeConfig(t, manager, ctx, "0", `{"value":"shared"}`)
	if _, err := manager.CompareAndSwapModuleConfig(ctx, compositeID, "0", json.RawMessage(`{"value":"stale"}`)); err == nil {
		t.Fatal("stale configuration CAS overwrote the module snapshot")
	}
	current, err := manager.ModuleConfig(ctx, compositeID)
	if err != nil || current.Version != config.Version || !bytes.Equal(current.Values, config.Values) {
		t.Fatalf("configuration CAS did not retain the winning snapshot: %+v, %v", current, err)
	}
	accountA, digestA := compositeAccount(t, manager, ctx, "account-a")
	accountB, digestB := compositeAccount(t, manager, ctx, "account-b")
	var primary, backend hybrid.Identity
	compositeAction(t, manager, ctx, "pid", accountA, `{}`, &primary)
	compositeAction(t, manager, ctx, "backend.pid", accountA, `{}`, &backend)
	compositeCheckIdentity(t, primary, "primary", "1.0.0", config.Version, "shared")
	compositeCheckIdentity(t, backend, "backend", "1.0.0", config.Version, "shared")
	t.Logf("hybrid nativePID=%d hostPID=%d externalGoPID=%d", primary.PID, os.Getpid(), backend.PID)
	for _, test := range []struct {
		scope  sdk.Scope
		digest string
		text   string
	}{
		{accountA, digestA, "only-a"},
		{accountB, digestB, "only-b"},
	} {
		test.scope.Operation, test.scope.Attempt = "forward-"+test.scope.Account, 1
		body, _ := json.Marshal(hybrid.Input{Text: test.text})
		var result hybrid.ForwardResult
		compositeAction(t, manager, ctx, "forward", test.scope, string(body), &result)
		for _, identity := range []hybrid.Identity{result.Primary, result.Backend} {
			if identity.Account != test.scope.Account || identity.Generation != test.scope.Generation ||
				identity.AuthFingerprint != test.digest || identity.Text != test.text {
				t.Fatalf("component crossed synthetic account authority: %+v", identity)
			}
		}
		if result.Backend.PID != backend.PID {
			t.Fatal("account selection started a separate module service")
		}
		operation, err := manager.Store().Operation(compositeID, test.scope.Operation)
		if err != nil || operation.State != "completed" || operation.Scope != test.scope {
			t.Fatalf("nested action lost the single root operation: %+v, %v", operation, err)
		}
	}
	operations, err := os.ReadDir(filepath.Join(root, "private", compositeID, "operations"))
	if err != nil || len(operations) != 2 {
		t.Fatalf("two writes created extra helper operation journals: %d, %v", len(operations), err)
	}
	for _, scope := range []sdk.Scope{accountA, accountB} {
		stored, err := manager.Store().StateGet(scope, "hybrid.last")
		expected := `{"text":"only-a"}`
		if scope.Account == accountB.Account {
			expected = `{"text":"only-b"}`
		}
		if err != nil || string(stored.Value) != expected {
			t.Fatalf("service host callback crossed account state: scope=%+v value=%s err=%v", scope, stored.Value, err)
		}
	}
	domainScope := accountA
	domainScope.Operation, domainScope.Attempt = "domain-outcome", 1
	var domain hybrid.Identity
	compositeAction(t, manager, ctx, "backend.run", domainScope, `{"text":"domain-payload"}`, &domain)
	operation, err := manager.Store().Operation(compositeID, domainScope.Operation)
	if domain.Outcome != "accepted" || err != nil || operation.State != "completed" || operation.Scope != domainScope {
		t.Fatalf("module domain outcome changed root completion: payload=%+v journal=%+v err=%v", domain, operation, err)
	}
	httpClient := compositeHTTPClient(t)
	status := compositeHTTPStatus(t, httpClient, ctx, backend)
	if status.PID != backend.PID || status.ConfigVersion != config.Version || status.Value != "shared" {
		t.Fatalf("readiness endpoint served another component snapshot: %+v", status)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, backend.Endpoint+"/events", nil)
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	compositeHTTPProof(t, response.Header, backend)
	scanner := bufio.NewScanner(response.Body)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" ||
		!scanner.Scan() || scanner.Text() != "event: ready" || !scanner.Scan() ||
		!strings.Contains(scanner.Text(), `"pid":`+strconv.Itoa(backend.PID)) {
		t.Fatal("service did not expose its declared SSE readiness stream")
	}
	eof := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, response.Body)
		eof <- err
	}()
	if err := manager.SetEnabled(ctx, compositeID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Call(ctx, compositeID, sdk.MethodModuleCall, sdk.Scope{},
		sdk.ModuleCallRequest{Action: "pid", Input: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("disabled composite module admitted a new action")
	}
	compositeReceive(t, ctx, eof)
	compositeAwaitClosed(t, ctx, backend.Endpoint)
	if err := manager.SetEnabled(ctx, compositeID, true); err != nil {
		t.Fatal(err)
	}
	var reopenedPrimary, reopenedBackend hybrid.Identity
	compositeAction(t, manager, ctx, "pid", accountA, `{}`, &reopenedPrimary)
	compositeAction(t, manager, ctx, "backend.pid", accountA, `{}`, &reopenedBackend)
	if reopenedPrimary.PID != primary.PID || reopenedPrimary.Instance == primary.Instance ||
		reopenedBackend.PID == backend.PID || reopenedBackend.Instance == backend.Instance {
		t.Fatalf("off/on did not reopen the native session and replace its owned process: before=%+v/%+v after=%+v/%+v",
			primary, backend, reopenedPrimary, reopenedBackend)
	}
	t.Logf("off/on kept native host PID=%d and replaced external Go PID=%d with PID=%d", reopenedPrimary.PID, backend.PID, reopenedBackend.PID)
	compositeCheckIdentity(t, reopenedBackend, "backend", "1.0.0", config.Version, "shared")
	if err := manager.catalog.Verify(entry); err != nil {
		t.Fatalf("service startup mutated its immutable package: %v", err)
	}
}

func compositeStreamAndCancel(t *testing.T, distribution, root string) {
	manager, ctx, _ := compositeManager(t, distribution, root)
	text := strings.Repeat("s", sdk.MaxChunkBytes+19)
	input, _ := json.Marshal(hybrid.Input{Text: text})
	var streamed []byte
	var result sdk.ToolResult
	if err := manager.Invoke(ctx, compositeID, sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "stream", Arguments: input}, &result, func(event Event) error {
			if event.Name == "data" {
				var chunk []byte
				if err := json.Unmarshal(event.Data, &chunk); err != nil {
					return err
				}
				streamed = append(streamed, chunk...)
			}
			return nil
		}); err != nil || string(streamed) != text {
		t.Fatalf("native group stream failed: bytes=%d, %v", len(streamed), err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	accepted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := manager.Call(waitCtx, compositeID, sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "wait", Arguments: json.RawMessage(`{}`)}, func(event Event) error {
				if event.Name == "accepted" {
					close(accepted)
				}
				return nil
			})
		done <- err
	}()
	compositeReceive(t, ctx, accepted)
	cancel()
	if err := compositeReceive(t, ctx, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("native group call lost cancellation: %v", err)
	}
	var backend hybrid.Identity
	compositeAction(t, manager, ctx, "backend.pid", sdk.Scope{}, `{}`, &backend)
	nestedCtx, cancelNested := context.WithCancel(ctx)
	defer cancelNested()
	go func() {
		_, err := manager.Call(nestedCtx, compositeID, sdk.MethodModuleCall, sdk.Scope{},
			sdk.ModuleCallRequest{Action: "forward", Input: json.RawMessage(`{"text":"nested-cancel","wait":true}`)}, nil)
		done <- err
	}()
	httpClient := compositeHTTPClient(t)
	compositeEventually(t, ctx, func() bool {
		status := compositeHTTPStatus(t, httpClient, ctx, backend)
		return status.Active == 1
	})
	cancelNested()
	if err := compositeReceive(t, ctx, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("nested service call lost cancellation: %v", err)
	}
	compositeEventually(t, ctx, func() bool {
		status := compositeHTTPStatus(t, httpClient, ctx, backend)
		return status.Active == 0
	})
}

func compositeFrozenGeneration(t *testing.T, v1, v2, root string) {
	manager, ctx, old := compositeManager(t, v1, root)
	oldConfig := compositeConfig(t, manager, ctx, "0", `{"value":"old"}`)
	lease, err := manager.Acquire(ctx, compositeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	type callResult struct {
		result sdk.ToolResult
		err    error
	}
	done := make(chan callResult, 1)
	go func() {
		defer lease.Close()
		var result sdk.ToolResult
		body, err := lease.Call(ctx, sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "forward", Arguments: json.RawMessage(`{"text":"frozen"}`)}, func(event Event) error {
				if event.Name == "data" {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
		if err == nil {
			err = json.Unmarshal(body, &result)
		}
		done <- callResult{result, err}
	}()
	compositeReceive(t, ctx, started)
	if lease.group == nil {
		t.Fatal("hybrid request did not acquire its component group")
	}
	lease.group.mu.Lock()
	helper := lease.group.components["backend"]
	helperStarted := helper != nil && (helper.client != nil || helper.starting != nil)
	lease.group.mu.Unlock()
	if helperStarted {
		t.Fatal("lazy old-generation helper started before the held request reached its binding")
	}
	select {
	case result := <-done:
		t.Fatalf("forward did not hold its stream acknowledgement: %v", result.err)
	default:
	}
	updated, err := manager.Install(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision == old.Revision || updated.Directory == old.Directory {
		t.Fatal("hybrid update overwrote its active immutable distribution")
	}
	newConfig := compositeConfig(t, manager, ctx, oldConfig.Version, `{"value":"new"}`)
	var newPrimary, newBackend hybrid.Identity
	compositeAction(t, manager, ctx, "pid", sdk.Scope{}, `{}`, &newPrimary)
	compositeAction(t, manager, ctx, "backend.pid", sdk.Scope{}, `{}`, &newBackend)
	compositeCheckIdentity(t, newPrimary, "primary", "2.0.0", newConfig.Version, "new")
	compositeCheckIdentity(t, newBackend, "backend", "2.0.0", newConfig.Version, "new")
	once.Do(func() { close(release) })
	finished := compositeReceive(t, ctx, done)
	if finished.err != nil {
		t.Fatalf("old in-flight native group lost its lazy service: %v (cause: %v)", finished.err, errors.Unwrap(finished.err))
	}
	var oldResult hybrid.ForwardResult
	nativeToolContent(t, finished.result, &oldResult)
	compositeCheckIdentity(t, oldResult.Primary, "primary", "1.0.0", oldConfig.Version, "old")
	compositeCheckIdentity(t, oldResult.Backend, "backend", "1.0.0", oldConfig.Version, "old")
	if oldResult.Backend.PID == newBackend.PID {
		t.Fatal("old lease lazily selected the new generation's helper")
	}
	compositeAwaitClosed(t, ctx, oldResult.Backend.Endpoint)
	if err := manager.catalog.Verify(old); err != nil {
		t.Fatalf("update removed or mutated a retained old distribution: %v", err)
	}
	// A configuration CAS alone also creates a new runtime group.
	lastConfig := compositeConfig(t, manager, ctx, newConfig.Version, `{"value":"config-only"}`)
	var configBackend hybrid.Identity
	compositeAction(t, manager, ctx, "backend.pid", sdk.Scope{}, `{}`, &configBackend)
	compositeCheckIdentity(t, configBackend, "backend", "2.0.0", lastConfig.Version, "config-only")
	if configBackend.PID == newBackend.PID {
		t.Fatal("configuration CAS retargeted an existing helper instance")
	}
	compositeAwaitClosed(t, ctx, newBackend.Endpoint)
	t.Logf("old lazy helper PID=%d version=%s config=%s; replacement PID=%d version=%s config=%s",
		oldResult.Backend.PID, oldResult.Backend.Version, oldResult.Backend.ConfigVersion,
		newBackend.PID, newBackend.Version, newBackend.ConfigVersion)
}

func compositePartialStartup(t *testing.T, distribution, root string) {
	manager, ctx, _ := compositeManager(t, distribution, root)
	failed := compositeConfig(t, manager, ctx, "0", `{"value":"failed","failStart":true}`)
	_, err := manager.Call(ctx, compositeID, sdk.MethodModuleCall, sdk.Scope{},
		sdk.ModuleCallRequest{Action: "backend.pid", Input: json.RawMessage(`{}`)}, nil)
	if err == nil {
		t.Fatal("service startup failure was advertised as ready")
	}
	match := regexp.MustCompile(`pid=([0-9]+) endpoint=(http://127\.0\.0\.1:[0-9]+)`).FindStringSubmatch(err.Error())
	if len(match) != 3 {
		t.Fatalf("fixture startup failure lost its cleanup evidence: %v", err)
	}
	compositeAwaitClosed(t, ctx, match[2])
	repaired := compositeConfig(t, manager, ctx, failed.Version, `{"value":"repaired"}`)
	var backend hybrid.Identity
	compositeAction(t, manager, ctx, "backend.pid", sdk.Scope{}, `{}`, &backend)
	compositeCheckIdentity(t, backend, "backend", "1.0.0", repaired.Version, "repaired")
	failedPID, _ := strconv.Atoi(match[1])
	if backend.PID == failedPID {
		t.Fatal("failed startup's process was reused after configuration repair")
	}
}

type compositeStatus struct {
	hybrid.Identity
	Active int64 `json:"active"`
}

func compositeHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			ip := net.ParseIP(host)
			if err != nil || ip == nil || !ip.IsLoopback() {
				return nil, fmt.Errorf("fixture refuses non-loopback network target %q", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("fixture refuses redirects")
		},
	}
}

func compositeHTTPStatus(t *testing.T, client *http.Client, ctx context.Context, expected hybrid.Identity) compositeStatus {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, expected.Endpoint+"/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	compositeHTTPProof(t, response.Header, expected)
	var status compositeStatus
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status) != nil {
		t.Fatal("service status did not return bounded JSON")
	}
	if status.PID != expected.PID || status.Instance != expected.Instance || status.Component != expected.Component ||
		status.ConfigVersion != expected.ConfigVersion {
		t.Fatalf("HTTP peer differs from its RPC component instance: %+v, want %+v", status, expected)
	}
	return status
}

func compositeHTTPProof(t *testing.T, header http.Header, expected hybrid.Identity) {
	t.Helper()
	for _, proof := range []struct{ name, value string }{
		{sdk.HeaderComponentInstance, expected.Instance},
		{sdk.HeaderComponent, expected.Component},
		{sdk.HeaderComponentConfig, expected.ConfigVersion},
	} {
		actual := header.Values(proof.name)
		if proof.value == "" || len(actual) != 1 || actual[0] != proof.value {
			t.Fatalf("HTTP service lacks its exact RPC instance proof: %s=%q, want %q", proof.name, actual, proof.value)
		}
	}
}

func compositeAwaitClosed(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()
	location, err := url.Parse(endpoint)
	if err != nil || !componentLoopbackEndpoint(endpoint) {
		t.Fatalf("fixture returned an invalid loopback endpoint %q", endpoint)
	}
	compositeEventually(t, ctx, func() bool {
		call, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		connection, err := (&net.Dialer{}).DialContext(call, "tcp", location.Host)
		if err != nil {
			return ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded)
		}
		_ = connection.Close()
		return false
	})
}

func compositeEventually(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			t.Fatal("hybrid fixture did not reach its expected state:", err)
		}
		if ready() {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("hybrid fixture did not reach its expected state:", ctx.Err())
		}
	}
}

func compositeReceive[T any](t *testing.T, ctx context.Context, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		t.Fatal("hybrid fixture did not finish:", ctx.Err())
		var zero T
		return zero
	}
}
