package gateway

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestExtensionToolOwnerReadBudget(test *testing.T) {
	fixture := installRefreshFixture(test, buildToolRefreshWorker(test), sdk.FullContract().Version.Minor, time.Hour, nil)
	ownerPath := toolFixtureOwnerPath(fixture)
	writeToolFixtureOwner(test, ownerPath, toolFixtureTransfer("fixture-owner"))
	ctx := test.Context()
	if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(`{}`)); err != nil {
		test.Fatal(err)
	}
	if result, err := fixture.run(ctx, refreshToolArgs); err != nil {
		test.Fatal(err)
	} else {
		checkToolOwnerResult(test, result, "selected", 1)
	}
	root := fixture.manager.Namespace()
	private := filepath.Join(root, "private", fixture.scope.Plugin)
	state, err := filepath.Glob(filepath.Join(private, "state", "*.json"))
	if err != nil || len(state) != 1 {
		test.Fatalf("config fixture: %v, %v", state, err)
	}
	files := []string{ownerPath, filepath.Join(root, "catalog.json"), state[0], filepath.Join(private, "accounts.json")}
	preflight := countToolOwnerReads(test, files, func() {
		prepared, args, err := extensionToolContext(ctx, refreshToolSpec, refreshToolArgs)
		revision, bound := extension.SubscriptionOwnerRevision(prepared)
		if err != nil || !bound || revision != "fixture-owner" ||
			extension.ToolScopeFrom(prepared) != fixture.scope || string(args) != `{"value":"hello"}` {
			test.Fatalf("preparation produced %q, %v, %+v, %s: %v", revision, bound, extension.ToolScopeFrom(prepared), args, err)
		}
	})
	full := countToolOwnerReads(test, files, func() {
		result, err := fixture.run(ctx, refreshToolArgs)
		if err != nil {
			test.Fatal(err)
		}
		checkToolOwnerResult(test, result, "selected", 1)
	})
	public := countToolOwnerReads(test, files, func() {
		result, err := fixture.run(ctx, json.RawMessage(`{"value":"hello"}`))
		if err != nil {
			test.Fatal(err)
		}
		checkToolOwnerResult(test, result, "", 0)
	})
	test.Logf("files=[owners,catalog,config,accounts] preflight=%v MCP=%v public=%v", preflight, full, public)
	if preflight[0] != 3 {
		test.Errorf("private preflight opened the owner file %d times, want one identity pin and one read per check", preflight[0])
	}
	if full[0] < preflight[0]+4 || public[0] != 0 {
		test.Errorf("private dispatch/publication must read current ownership and public tools remain unbound: preflight=%v MCP=%v public=%v", preflight, full, public)
	}
	if fixture.toolCalls.Load() != 2 || fixture.refreshes.Load() != 0 {
		test.Errorf("private tool calls=%d refreshes=%d", fixture.toolCalls.Load(), fixture.refreshes.Load())
	}
}

func TestExtensionToolOwnerReadFences(test *testing.T) {
	worker := buildToolRefreshWorker(test)
	test.Run("legacy SDK 1.0", func(test *testing.T) {
		fixture := installRefreshFixture(test, worker, 0, time.Hour, nil)
		result, err := fixture.run(test.Context(), refreshToolArgs)
		if err != nil {
			test.Fatal(err)
		}
		checkToolOwnerResult(test, result, "selected", 1)
		if fixture.toolCalls.Load() != 1 || fixture.refreshes.Load() != 0 {
			test.Errorf("legacy tool calls=%d refreshes=%d", fixture.toolCalls.Load(), fixture.refreshes.Load())
		}
	})
	test.Run("ordinary MCP", func(test *testing.T) {
		fixture := installRefreshFixture(test, worker, sdk.FullContract().Version.Minor, time.Hour, nil)
		ownerPath := toolFixtureOwnerPath(fixture)
		entry, config, err := fixture.manager.ActiveModuleConfig(test.Context(), fixture.scope.Plugin)
		if err != nil {
			test.Fatal(err)
		}
		selected, err := extension.NewToolAccountSelection(fixture.manager.Namespace(), entry, config)
		if err != nil {
			test.Fatal(err)
		}
		for _, change := range []struct{ name, current, replacement string }{
			{"new owner after unowned preparation", `[]`, toolFixtureTransfer("fixture-owner")},
			{"owner replaced after transferred preparation", toolFixtureTransfer("fixture-owner"), toolFixtureTransfer("another-owner")},
			{"corrupt owner after unowned preparation", `[]`, `[`},
		} {
			test.Run(change.name, func(test *testing.T) {
				writeToolFixtureOwner(test, ownerPath, change.current)
				original := runExtensionPluginTool
				defer func() { runExtensionPluginTool = original }()
				var operation string
				var replaced bool
				runExtensionPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
					operation = extension.ToolScopeFrom(ctx).Operation
					ctx = extension.WithToolAccountCheck(ctx, selected, func(context.Context) error {
						if !replaced {
							writeToolFixtureOwner(test, ownerPath, change.replacement)
							replaced = true
						}
						return nil
					})
					return original(ctx, spec, name, args)
				}
				ctx := extension.WithRequestMetadata(test.Context(), extension.RequestMetadata{
					Plugin: fixture.scope.Plugin, RequestID: test.Name(),
				})
				calls := fixture.toolCalls.Load()
				_, err := fixture.run(ctx, refreshToolArgs)
				var outcome *extension.OutcomeError
				if !replaced || operation == "" || !errors.As(err, &outcome) || outcome.Outcome != "not-started" || fixture.toolCalls.Load() != calls {
					test.Fatalf("stale authority: operation=%q calls=%d -> %d err=%v", operation, calls, fixture.toolCalls.Load(), err)
				}
				receipt, err := fixture.manager.Store().Operation(fixture.scope.Plugin, operation)
				if err != nil || receipt.State != "not-started" {
					test.Fatalf("receipt=%+v err=%v", receipt, err)
				}
				runExtensionPluginTool = original
				writeToolFixtureOwner(test, ownerPath, toolFixtureTransfer("repaired-owner"))
				result, err := fixture.run(test.Context(), refreshToolArgs)
				if err != nil {
					test.Fatal(err)
				}
				checkToolOwnerResult(test, result, "selected", 1)
				if fixture.toolCalls.Load() != calls+1 || fixture.refreshes.Load() != 0 {
					test.Errorf("fresh authority: calls=%d -> %d, refreshes=%d", calls, fixture.toolCalls.Load(), fixture.refreshes.Load())
				}
			})
		}
	})
}

func checkToolOwnerResult(test *testing.T, result map[string]any, account string, generation uint64) {
	test.Helper()
	blocks, ok := result["content"].([]any)
	if !ok || len(blocks) != 1 || result["isError"] != false {
		test.Fatalf("unexpected MCP result %+v", result)
	}
	block, _ := blocks[0].(map[string]any)
	body, _ := block["text"].(string)
	var call struct {
		Scope sdk.Scope       `json:"scope"`
		Auth  json.RawMessage `json:"auth"`
		Args  sdk.ToolRequest `json:"params"`
	}
	err := json.Unmarshal([]byte(body), &call)
	if err != nil || call.Scope.Plugin != "tool.refresh" || call.Scope.Account != account || call.Scope.Generation != generation ||
		call.Args.ID != "echo" || string(call.Args.Arguments) != `{"value":"hello"}` {
		test.Fatalf("tool received unexpected request: %s, %v", body, err)
	}
	if account == "" {
		if call.Scope.Provider != "" || string(call.Auth) != "null" {
			test.Fatalf("unbound tool received account authority: %s", body)
		}
	} else if call.Scope.Provider != "vendor" || !strings.Contains(string(call.Auth), `"token":"synthetic-original"`) {
		test.Fatalf("private tool lost its account authority: %s", body)
	}
}

func toolFixtureOwnerPath(fixture *toolRefreshFixture) string {
	return filepath.Join(fixture.manager.Namespace(), "private", ".saved-signins", "subscription-owners.json")
}

func toolFixtureTransfer(revision string) string {
	return `[{"module":"tool.refresh","provider":"vendor","publicID":"tool-fixture","revision":"` + revision + `","active":true,
		"accounts":[{"key":"fixture-key","id":"selected","user":"fixture-user","loginID":"fixture-login"}],
		"grantRevision":"` + strings.Repeat("a", 64) + `","digests":{"fixture-key":"` + strings.Repeat("b", 64) + `"},"tickets":["fixture-ticket"]}]`
}

func writeToolFixtureOwner(test *testing.T, path, body string) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		test.Fatal(err)
	}
	temporary := path + ".fixture.tmp"
	if err := os.WriteFile(temporary, []byte(body), 0600); err != nil {
		test.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		test.Fatal(err)
	}
}

func countToolOwnerReads(test *testing.T, files []string, call func()) []int {
	test.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		test.Fatal(err)
	}
	defer unix.Close(fd)
	watches := make(map[uint32]int, len(files))
	for index, file := range files {
		watch, err := unix.InotifyAddWatch(fd, file, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE)
		if err != nil {
			test.Fatal(err)
		}
		watches[uint32(watch)] = index
	}
	call()
	counts := make([]int, len(files))
	var events [4096]byte
	for {
		size, err := unix.Read(fd, events[:])
		switch err {
		case unix.EAGAIN:
			return counts
		case unix.EINTR:
			continue
		}
		if err != nil || size == 0 || size%unix.SizeofInotifyEvent != 0 {
			test.Fatalf("reading tool file events: %d bytes, %v", size, err)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			watch := binary.NativeEndian.Uint32(events[offset:])
			mask := binary.NativeEndian.Uint32(events[offset+4:])
			index, ok := watches[watch]
			if !ok || mask != unix.IN_OPEN && mask != unix.IN_CLOSE_NOWRITE {
				test.Fatalf("unexpected tool file event: watch=%d mask=%#x", watch, mask)
			}
			if mask == unix.IN_CLOSE_NOWRITE {
				counts[index]++
			}
		}
	}
}
