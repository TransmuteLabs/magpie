package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolHTTPHandoffBlocksCLIRollback(test *testing.T) {
	cli := buildToolRollbackCLI(test)
	fixture := installRefreshFixture(test, buildToolRefreshWorker(test), sdk.FullContract().Version.Minor, time.Hour, nil)
	const ownerRevision = "fixture-owner"
	publicID := seedToolFixtureRollback(test, fixture, ownerRevision)
	ownerPath := toolFixtureOwnerPath(fixture)
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(testenv.DeadlineContext(test, 5*time.Second), 30*time.Second)
	defer cancel()

	entry, config, err := fixture.manager.ActiveModuleConfig(ctx, fixture.scope.Plugin)
	if err != nil {
		test.Fatal(err)
	}
	selection, err := extension.NewToolAccountSelection(fixture.manager.Namespace(), entry, config)
	if err != nil {
		test.Fatal(err)
	}
	reached, proceed := make(chan struct{}), make(chan struct{})
	original := runExtensionPluginTool
	runExtensionPluginTool = func(current context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
		revision, _ := extension.SubscriptionOwnerRevision(current)
		if revision != ownerRevision || extension.ToolScopeFrom(current) != fixture.scope {
			return sdk.ToolResult{}, fmt.Errorf("wrong tool authority before final admission: %q, %+v", revision, extension.ToolScopeFrom(current))
		}
		var checks atomic.Int32
		current = extension.WithToolAccountCheck(current, selection, func(admission context.Context) error {
			if checks.Add(1) != 3 {
				return nil
			}
			close(reached)
			select {
			case <-proceed:
				return nil
			case <-admission.Done():
				return admission.Err()
			}
		})
		return original(current, spec, name, args)
	}
	server := httptest.NewServer(New().Front())
	var work sync.WaitGroup
	defer func() {
		cancel()
		server.Close()
		work.Wait()
		runExtensionPluginTool = original
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+ToolsMCPPath,
		strings.NewReader(toolsCall(fixture.tool.name, string(refreshToolArgs))))
	if err != nil {
		test.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	var reply mcpReply
	toolDone := make(chan error, 1)
	work.Go(func() {
		response, err := server.Client().Do(request)
		if err != nil {
			toolDone <- err
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
			toolDone <- fmt.Errorf("unexpected MCP response: %s (%s)", response.Status, response.Header.Get("Content-Type"))
			return
		}
		toolDone <- json.NewDecoder(response.Body).Decode(&reply)
	})
	select {
	case <-reached:
	case err := <-toolDone:
		test.Fatalf("HTTP tool finished before final admission: %+v, %v", reply, err)
	case <-ctx.Done():
		test.Fatalf("HTTP tool did not reach final admission: %v", ctx.Err())
	}
	receipt := readToolRollbackReceipt(test, fixture)
	scope := fixture.scope
	scope.Operation, scope.Attempt = receipt.Scope.Operation, 1
	if scope.Operation == "" || receipt.Scope != scope || receipt.State != "dispatched" {
		test.Fatalf("tool did not journal its selected dispatch before the Store wait: %+v", receipt)
	}

	release, held := filelock.Lock(filepath.Join(fixture.manager.Namespace(), "private", ".lock"), time.Second)
	if !held {
		test.Fatal("could not lock the fixture's private Store")
	}
	release = sync.OnceFunc(release)
	defer release()
	close(proceed)
	waitToolHandoffStack(test, ctx, toolDone, "HTTP tool waiting for the final private Store check",
		"(*Server).toolsMCPRun(", "(*Lease).admitToolHandoff(", "storeWaitLock(")
	catalog, err := os.Open(filepath.Join(fixture.manager.Namespace(), "catalog.lock"))
	if err != nil {
		test.Fatal(err)
	}
	defer catalog.Close()
	catalogFree := func() bool {
		shared, err := filelock.TryShared(catalog)
		if err != nil {
			test.Fatal(err)
		}
		if shared {
			filelock.Unlock(catalog)
		}
		return shared
	}
	if !catalogFree() {
		test.Fatal("the catalog shared lock was unavailable before CLI rollback")
	}
	home := os.Getenv("HOME")
	rollback := func() ([]byte, error) {
		command := proc.CommandContext(ctx, cli, "plugin", "sdk", "auth", "rollback", publicID, ownerRevision)
		command.Dir = home
		command.Env = append(os.Environ(), "MAGPIE_CLI_TEST_SERVE=1", "XDG_CONFIG_DIRS="+filepath.Join(home, "etc", "xdg"),
			"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"), "XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
			"XDG_RUNTIME_DIR="+filepath.Join(home, "run"))
		return command.Output()
	}
	var output []byte
	var rollbackErr error
	rollbackDone := make(chan struct{})
	work.Go(func() {
		defer close(rollbackDone)
		output, rollbackErr = rollback()
	})
	waitToolRefreshPhase(test, ctx, "CLI rollback holding the catalog writer while the Store is locked", func() bool {
		select {
		case <-rollbackDone:
			test.Fatalf("CLI rollback exited before reaching Store contention: stdout=%q, error=%v, stderr=%q",
				output, rollbackErr, cliStderr(rollbackErr))
		default:
		}
		return !catalogFree()
	})
	if fixture.toolCalls.Load() != 0 || fixture.refreshes.Load() != 0 {
		test.Fatal("a selected tool or refresh reached the worker before Store release")
	}
	release()
	select {
	case <-rollbackDone:
	case <-ctx.Done():
		test.Fatalf("busy CLI rollback did not return: %v", ctx.Err())
	}
	const busy = "magpie: the SDK subscription is busy; try again when its current operations finish"
	var exit *exec.ExitError
	if !errors.As(rollbackErr, &exit) || exit.ExitCode() != 1 || strings.TrimSpace(string(exit.Stderr)) != busy || len(output) != 0 {
		test.Fatalf("CLI rollback did not exit busy without a receipt: stdout=%q, error=%v, stderr=%q", output, rollbackErr, cliStderr(rollbackErr))
	}
	ownerAfter, err := os.ReadFile(ownerPath)
	if err != nil || !bytes.Equal(ownerBefore, ownerAfter) {
		test.Fatalf("busy CLI rollback changed the owner file: %v", err)
	}
	activeID, revision, user, visible := provider.ExtensionPublicLogin(scope)
	if !visible || activeID != publicID || revision != ownerRevision || user != "fixture-user" {
		test.Fatalf("busy CLI rollback changed the SDK login: %q %q %q visible=%t", activeID, revision, user, visible)
	}

	select {
	case err := <-toolDone:
		if err != nil {
			test.Fatal(err)
		}
	case <-ctx.Done():
		test.Fatalf("HTTP tool did not finish: %v", ctx.Err())
	}
	content := reply.Result.Content
	if string(reply.ID) != "3" || reply.Error != nil || reply.Result.IsError || len(content) != 1 || content[0].Type != "text" {
		test.Fatalf("HTTP tool did not return a successful JSON-RPC result: %+v", reply)
	}
	var workerReply struct {
		Scope sdk.Scope `json:"scope"`
	}
	err = json.Unmarshal([]byte(content[0].Text), &workerReply)
	receipt = readToolRollbackReceipt(test, fixture)
	if err != nil || workerReply.Scope != scope || receipt.Scope != scope || receipt.State != "completed" ||
		fixture.toolCalls.Load() != 1 || fixture.refreshes.Load() != 0 || fixture.executes.Load() != 0 {
		test.Fatalf("HTTP tool completion: worker scope=%+v, journal=%+v, tool=%d refresh=%d execute=%d, err=%v",
			workerReply.Scope, receipt, fixture.toolCalls.Load(), fixture.refreshes.Load(), fixture.executes.Load(), err)
	}

	output, err = rollback()
	var owner extension.SubscriptionOwner
	if err != nil {
		test.Fatalf("repeated CLI rollback failed: %v %q", err, cliStderr(err))
	}
	if err := json.Unmarshal(output, &owner); err != nil || owner.Active || owner.Pending || owner.PublicID != publicID ||
		owner.Module != scope.Plugin || owner.Provider != scope.Provider || owner.Revision == "" || owner.Revision == ownerRevision {
		test.Fatalf("repeated CLI rollback did not publish a new inactive owner: %+v, %v", owner, err)
	}
	_, revision, _, visible = provider.ExtensionPublicLogin(scope)
	community, restored := provider.PluginOf(publicID)
	if visible || revision != owner.Revision || !restored || !community.SignedIn || community.ID != scope.Provider ||
		len(community.Accounts) != 1 || community.Accounts[0].AccountID != user {
		test.Fatalf("CLI rollback did not restore the same Community account: SDK revision=%q visible=%t, Community restored=%t signed-in=%t",
			revision, visible, restored, community.SignedIn)
	}
}

func readToolRollbackReceipt(test *testing.T, fixture *toolRefreshFixture) extension.JournalEntry {
	test.Helper()
	directory := filepath.Join(fixture.manager.Namespace(), "private", fixture.scope.Plugin, "operations")
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 {
		test.Fatalf("expected one isolated tool receipt, got %d: %v", len(files), err)
	}
	data, err := os.ReadFile(filepath.Join(directory, files[0].Name()))
	if err != nil {
		test.Fatal(err)
	}
	var receipt extension.JournalEntry
	if err := json.Unmarshal(data, &receipt); err != nil {
		test.Fatal(err)
	}
	return receipt
}

func buildToolRollbackCLI(test *testing.T) string {
	test.Helper()
	entry := "magpie-cli"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	executable := filepath.Join(test.TempDir(), entry)
	build := proc.CommandContext(testenv.DeadlineContext(test, 35*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"test", "-c", "-mod=readonly", "-tags", "nogui", "-o", executable, "../cli")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build offline CLI fixture: %v\n%s", err, output)
	}
	return executable
}

func cliStderr(err error) []byte {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.Stderr
	}
	return nil
}
