package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	if slices.Contains(os.Args, "--owned-pipe-child") {
		ownedPipeChild()
		os.Exit(0)
	}
}

func ownedPipeChild() {
	if slices.Contains(os.Args, "--block-stdin") {
		_, _ = os.Stdout.Write([]byte("ready\n"))
		stopped := make(chan os.Signal, 1)
		signal.Notify(stopped, os.Interrupt)
		<-stopped
		return
	}
	_, _ = os.Stderr.Write([]byte("private-token=NEVER-LOG-THIS\n"))
	_, _ = os.Stdout.Write([]byte("ready\n"))
	command := make([]byte, 1)
	for {
		if _, err := io.ReadFull(os.Stdin, command); err != nil {
			return
		}
		switch command[0] {
		case 'R':
			_, _ = os.Stdout.Write([]byte("reply\n"))
		case 'L':
			_, _ = os.Stdout.Write(ownedLargeBytes())
		case 'X':
			return
		}
	}
}

func ownedLargeBytes() []byte {
	return bytes.Repeat([]byte("split-世界-\u2028-\u2029\r\n"), 70000)
}

type ownedFixture struct {
	manager *Manager
	lease   *Lease
	scope   sdk.Scope
	parent  *ownedProcess
	ctx     context.Context
	cancel  context.CancelFunc
}

func newOwnedFixture(t *testing.T, arguments ...string) *ownedFixture {
	return newOwnedFixtureWith(t, nil, arguments...)
}

func newOwnedFixtureWith(t *testing.T, configure func(*OwnedProcessLaunch), arguments ...string) *ownedFixture {
	t.Helper()
	manager, scope := usageManagerVersion(t, 8)
	scope.Operation, scope.Attempt, scope.Session = "owned-process", 1, "host-session"
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), filepath.Base(executable))
	if err := os.Symlink(executable, alias); err != nil {
		source, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		output, err := os.OpenFile(alias, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(output, source)
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatal(copyErr, closeErr)
		}
	}
	executable = alias
	launch := OwnedProcessLaunch{
		Recipe:     sdk.AgentProcessRecipe{ID: "native", Agent: "agent", Argv: append([]string{"--owned-pipe-child"}, arguments...)},
		Executable: executable, Directory: t.TempDir(), Env: []string{"OWNED_FIXTURE=1"},
		Profile: "owned-profile", Revision: "1", RouteID: 93,
		Check: func(ctx context.Context) error { return ctx.Err() },
	}
	if configure != nil {
		configure(&launch)
	}
	identity, err := captureProcessFile(launch.Executable)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Stat(launch.Directory)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := newOwnedProcess(ctx, lease, scope, launch, identity, directory)
	if err != nil {
		t.Fatal(err)
	}
	parent.component, parent.parentID = sdk.PrimaryComponent, "owned-parent"
	t.Cleanup(parent.finish)
	if _, err := manager.store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.store.Journal(scope, "accepted"); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, processContextKey{}, parent)
	ctx = lease.group.withAuthority(ctx, sdk.PrimaryComponent, "write")
	return &ownedFixture{manager: manager, lease: lease, scope: scope, parent: parent, ctx: ctx, cancel: cancel}
}

func (fixture *ownedFixture) host(method string, value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return fixture.manager.processHostCall(fixture.ctx, fixture.scope, method, raw,
		hostAuthority{manifest: fixture.lease.Manifest(), method: sdk.MethodModuleCall})
}

func (fixture *ownedFixture) open(t *testing.T) sdk.ProcessOpenResult {
	t.Helper()
	value, err := fixture.host(sdk.MethodProcessOpen, sdk.ProcessOpenRequest{RecipeID: "native"})
	if err != nil {
		t.Fatal("host failed to open its owned real child", err)
	}
	return value.(sdk.ProcessOpenResult)
}

func (fixture *ownedFixture) read(t *testing.T, pipe string, seq uint64, limit int) sdk.ProcessReadResult {
	t.Helper()
	value, err := fixture.host(sdk.MethodProcessRead, sdk.ProcessReadRequest{
		Handle: fixture.parent.handle, Pipe: pipe, Seq: strconv.FormatUint(seq, 10), MaxBytes: limit,
	})
	if err != nil {
		t.Fatal("owned output read failed", err)
	}
	return value.(sdk.ProcessReadResult)
}

func (fixture *ownedFixture) write(t *testing.T, seq uint64, value string) {
	t.Helper()
	result, err := fixture.host(sdk.MethodProcessWrite, sdk.ProcessWriteRequest{
		Handle: fixture.parent.handle, WriteID: "write-" + strconv.FormatUint(seq, 10),
		Seq: strconv.FormatUint(seq, 10), Bytes: []byte(value),
	})
	if err != nil || result.(sdk.ProcessWriteResult).Delivery != "written" {
		t.Fatal("owned input delivery failed", result, err)
	}
}

func TestOwnedProcessRealChildSeparatePipesAndBoundedLosslessBytes(t *testing.T) {
	fixture := newOwnedFixture(t)
	opened := fixture.open(t)
	if opened.Handle == "" || opened.Incarnation == "" || opened.Handle == opened.Incarnation ||
		opened.Limits.MaxChunkBytes != sdk.MaxChunkBytes || opened.Limits.QueueChunks != 2 {
		t.Fatalf("opaque process admission lost its bounded identity: %+v", opened)
	}
	if got := fixture.read(t, "stdout", 1, 64); string(got.Bytes) != "ready\n" || got.EOF {
		t.Fatalf("stdout was merged, truncated or closed: %+v", got)
	}
	if got := fixture.read(t, "stderr", 1, 64); string(got.Bytes) != "private-token=NEVER-LOG-THIS\n" || got.EOF {
		t.Fatalf("stderr did not retain its separate private bytes: %+v", got)
	}
	result := make(chan error, 1)
	go func() {
		value, err := fixture.host(sdk.MethodProcessRead, sdk.ProcessReadRequest{
			Handle: opened.Handle, Pipe: "stdout", Seq: "2", MaxBytes: 64,
		})
		if err == nil && string(value.(sdk.ProcessReadResult).Bytes) != "reply\n" {
			err = errors.New("pending stdout did not receive the stdin-triggered reply")
		}
		result <- err
	}()
	fixture.write(t, 1, "R")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	fixture.write(t, 2, "L")
	expected := ownedLargeBytes()
	var received []byte
	for seq := uint64(3); len(received) < len(expected); seq++ {
		chunk := fixture.read(t, "stdout", seq, sdk.MaxChunkBytes-17)
		if chunk.EOF || len(chunk.Bytes) == 0 || len(chunk.Bytes) > sdk.MaxChunkBytes-17 {
			t.Fatalf("large output lost bounded backpressure: %+v", chunk)
		}
		received = append(received, chunk.Bytes...)
	}
	if !bytes.Equal(received, expected) {
		t.Fatal("large split UTF-8/CRLF/U+2028/U+2029 output changed bytes")
	}
	if _, err := fixture.host(sdk.MethodProcessStdinEOF, sdk.ProcessHandleRequest{Handle: opened.Handle}); err != nil {
		t.Fatal(err)
	}
	value, err := fixture.host(sdk.MethodProcessClose, sdk.ProcessHandleRequest{Handle: opened.Handle})
	if err != nil || !value.(sdk.ProcessCloseResult).Joined {
		t.Fatal("process close returned before joining", value, err)
	}
}

func TestOwnedProcessRejectsForeignParentHandleAndWriteReplay(t *testing.T) {
	fixture := newOwnedFixture(t)
	opened := fixture.open(t)
	_ = fixture.read(t, "stdout", 1, 64)
	if _, err := fixture.host(sdk.MethodProcessRead, sdk.ProcessReadRequest{
		Handle: "foreign", Pipe: "stdout", Seq: "1", MaxBytes: 64,
	}); err == nil {
		t.Fatal("a foreign process handle acquired this parent's pipe")
	}
	fixture.write(t, 1, "R")
	replay := sdk.ProcessWriteRequest{Handle: opened.Handle, WriteID: "write-1", Seq: "2", Bytes: []byte("R")}
	if _, err := fixture.host(sdk.MethodProcessWrite, replay); err == nil {
		t.Fatal("an admitted write ID was replayed")
	}
	replay.WriteID, replay.Seq = "another", "1"
	if _, err := fixture.host(sdk.MethodProcessWrite, replay); err == nil {
		t.Fatal("an admitted input sequence was replayed")
	}
	raw, _ := json.Marshal(sdk.ProcessHandleRequest{Handle: opened.Handle})
	if _, err := fixture.manager.processHostCall(context.Background(), fixture.scope, sdk.MethodProcessClose, raw,
		hostAuthority{manifest: fixture.lease.Manifest(), method: sdk.MethodModuleCall}); err == nil {
		t.Fatal("a cached handle outlived its parent authority")
	}
	if _, err := fixture.parent.open(fixture.ctx, "native"); err == nil {
		t.Fatal("an admitted process was launched twice")
	}
}

func TestOwnedProcessRevocationClosesAndJoinsRealChild(t *testing.T) {
	for _, reason := range []string{"parent", "lease", "off", "config", "account", "profile", "executable"} {
		t.Run(reason, func(t *testing.T) {
			var current atomic.Bool
			current.Store(true)
			fixture := newOwnedFixtureWith(t, func(launch *OwnedProcessLaunch) {
				launch.Check = func(ctx context.Context) error {
					if !current.Load() {
						return errors.New("profile revoked")
					}
					return ctx.Err()
				}
			}, "--block-stdin")
			fixture.open(t)
			_ = fixture.read(t, "stdout", 1, 64)
			readDone := make(chan error, 1)
			go func() {
				_, err := fixture.host(sdk.MethodProcessRead, sdk.ProcessReadRequest{
					Handle: fixture.parent.handle, Pipe: "stdout", Seq: "2", MaxBytes: 64,
				})
				readDone <- err
			}()
			switch reason {
			case "parent":
				fixture.cancel()
			case "lease":
				fixture.lease.Close()
			case "off":
				if err := fixture.manager.SetEnabled(fixture.ctx, fixture.scope.Plugin, false); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := fixture.manager.CompareAndSwapModuleConfig(fixture.ctx, fixture.scope.Plugin,
					fixture.lease.ConfigVersion(), json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "account":
				account, err := fixture.manager.store.GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					t.Fatal(err)
				}
				account.Auth = json.RawMessage(`{"token":"replacement"}`)
				if _, err := fixture.manager.store.SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "profile":
				current.Store(false)
				if err := os.WriteFile(filepath.Join(fixture.parent.launch.Directory, "profile-change"), []byte("revoked"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "executable":
				if err := os.Remove(fixture.parent.executable.original); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture.parent.executable.original, []byte("replaced"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-fixture.parent.closed:
			case <-fixture.ctx.Done():
				if reason != "parent" {
					t.Fatal("revocation did not join its process before the finite parent ended")
				}
				<-fixture.parent.closed
			}
			if err := <-readDone; err == nil {
				t.Fatal("revoked output was published")
			}
			if !fixture.parent.joined {
				t.Fatal("revocation left process pumps alive")
			}
		})
	}
}

func TestOwnedProcessExitAndAcknowledgmentCannotCompleteFinancialOutcome(t *testing.T) {
	for _, terminal := range []string{`{}`, `{"ack":true}`, `{"outcome":"accepted"}`, `{"outcome":"unknown"}`, `{"outcome":"completed"}`} {
		t.Run(terminal, func(t *testing.T) {
			fixture := newOwnedFixture(t)
			fixture.open(t)
			fixture.write(t, 1, "X")
			<-fixture.parent.waitDone
			err := fixture.parent.result(json.RawMessage(terminal), nil)
			if terminal == `{"outcome":"completed"}` {
				if err != nil {
					t.Fatal("explicit owned terminal was lost", err)
				}
			} else {
				var outcome *OutcomeError
				if !errors.As(err, &outcome) || outcome.Outcome != "uncertain" {
					t.Fatal("ACK, unknown or exit0 fabricated financial completion", err)
				}
			}
		})
	}
}

func TestOwnedProcessAuthorityRecheckedAfterAdmissionBarrier(t *testing.T) {
	admitted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var current atomic.Bool
	current.Store(true)
	fixture := newOwnedFixtureWith(t, func(launch *OwnedProcessLaunch) {
		launch.Check = func(ctx context.Context) error {
			if !current.Load() {
				return errors.New("generation changed")
			}
			return ctx.Err()
		}
		launch.Admit = func(ctx context.Context) (func(), error) {
			close(admitted)
			select {
			case <-release:
				return func() {}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := fixture.parent.open(fixture.ctx, "native")
		done <- err
	}()
	<-admitted
	current.Store(false)
	once.Do(func() { close(release) })
	if err := <-done; err == nil {
		t.Fatal("changed authority spawned after waiting for admission")
	}
	if fixture.parent.tree != nil {
		t.Fatal("revoked admission started a child")
	}
}

func TestOwnedProcessSpawnReceiptSurvivesOwnerLossWithoutReplay(t *testing.T) {
	fixture := newOwnedFixture(t)
	fixture.open(t)
	fixture.parent.close("transport-lost")
	_, err := fixture.parent.lease.manager.store.Operation(fixture.scope.Plugin, fixture.scope.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.parent.journal("spawn-intent", "", 0, 0, true); err == nil {
		t.Fatal("a durable process admission was replayed after transport loss")
	}
	err = fixture.manager.store.withStore(fixture.scope.Plugin, func(root *os.Root) error {
		directory, err := root.OpenRoot("processes")
		if err != nil {
			return err
		}
		defer directory.Close()
		body, err := directory.ReadFile(storeHash(fixture.scope.Operation) + ".json")
		if err == nil && (strings.Contains(string(body), "NEVER-LOG-THIS") || strings.Contains(string(body), "--owned-pipe-child")) {
			return errors.New("transport journal retained private bytes or arguments")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
