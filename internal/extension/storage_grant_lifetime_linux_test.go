package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func storageExternalLockAvailable(t *testing.T, name string) bool {
	t.Helper()
	command := proc.CommandContext(t.Context(), "python3", "-c", `
import errno, fcntl, os, sys
fd = os.open(sys.argv[1], os.O_RDWR)
try:
    fcntl.lockf(fd, fcntl.LOCK_EX | fcntl.LOCK_NB, 510, 0x40000002)
except OSError as error:
    if error.errno not in (errno.EACCES, errno.EAGAIN):
        raise
    print("held")
else:
    print("available")
finally:
    os.close(fd)
`, name)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("external lock probe failed", string(output), err)
	}
	switch string(output) {
	case "available\n":
		return true
	case "held\n":
		return false
	default:
		t.Fatal("invalid external lock probe", string(output))
		return false
	}
}

func TestStorageMetadataRecheckDoesNotClosePOSIXLockedInode(t *testing.T) {
	grant, ctx, directory := storageGrantFixture(t)
	name := filepath.Join(directory, "fixture.db")
	if err := os.WriteFile(name, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lock := unix.Flock_t{Type: unix.F_RDLCK, Start: 0x40000002, Len: 510}
	if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	grant.files["native:fixture.db"] = &storageFile{name: "fixture.db", info: info}
	if err := grant.Recheck(ctx); err != nil {
		t.Fatal("metadata-only native recheck failed", err)
	}
	if storageExternalLockAvailable(t, name) {
		t.Fatal("broker metadata probe released an active process-owned SQLite lock")
	}
	probe, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	if !storageExternalLockAvailable(t, name) {
		t.Fatal("external probe did not demonstrate POSIX close semantics")
	}
}

func TestStorageMetadataIdentityAndNoFollowSurvivePublication(t *testing.T) {
	grant, ctx, directory := storageGrantFixture(t)
	name := filepath.Join(directory, "fixture.db")
	if err := os.WriteFile(name, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := grant.stat("fixture.db")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(name)
	if err != nil || !storageSameFile(selected, actual) || selected.Mode() != actual.Mode() ||
		selected.Size() != actual.Size() || !selected.ModTime().Equal(actual.ModTime()) {
		t.Fatal("rooted metadata disagrees with the selected physical inode", selected, actual, err)
	}
	grant.files["native:fixture.db"] = &storageFile{name: "fixture.db", info: selected}
	if err := grant.Recheck(ctx); err != nil {
		t.Fatal("unchanged metadata-only selection was rejected", err)
	}
	if err := os.Rename(name, name+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, selected.ModTime(), selected.ModTime()); err != nil {
		t.Fatal(err)
	}
	replaced, err := grant.stat("fixture.db")
	if err != nil || storageSameFile(selected, replaced) || grant.Recheck(ctx) == nil {
		t.Fatal("equal file metadata hid a replaced database inode", err)
	}
	if err := os.Symlink(name, filepath.Join(directory, "fixture.db-wal")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(directory, "fixture.db-shm"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"fixture.db-wal", "fixture.db-shm", "../fixture.db"} {
		if _, err := grant.stat(member); err == nil {
			t.Fatal("metadata probe accepted an unsafe finite member", member)
		}
	}
}

func TestStorageCanceledLifetimeWaitsForActualParentTerminal(t *testing.T) {
	grant, ctx, directory := storageGrantFixture(t)
	serial := make(chan struct{}, 1)
	lifetime, err := acquireStorageSerial(t.Context(), serial)
	if err != nil {
		t.Fatal(err)
	}
	grant.lifetime = lifetime
	trackStorageCall(ctx)
	lifetime.native = true
	name := filepath.Join(directory, "fixture.db")
	if err := os.WriteFile(name, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := grant.open("fixture.db")
	if err != nil {
		t.Fatal(err)
	}
	lock := unix.Flock_t{Type: unix.F_RDLCK, Start: 0x40000002, Len: 510}
	if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock); err != nil {
		t.Fatal(err)
	}
	if err := grant.closeMember(file); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := grant.Recheck(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read remained publishable", err)
	}
	if err := grant.Close(); err != nil {
		t.Fatal(err)
	}
	if len(serial) != 1 || storageExternalLockAvailable(t, name) {
		t.Fatal("cancellation released native locks before the handler terminal")
	}
	endStorageCall(ctx)
	if len(serial) != 0 || !storageExternalLockAvailable(t, name) {
		t.Fatal("terminal result did not release native descriptors and admission")
	}
	if err := grant.Close(); err != nil {
		t.Fatal("close was not idempotent", err)
	}
}

func TestStorageCanceledWireReleasesOnlyAfterValidTerminal(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint("valid=", valid), func(t *testing.T) {
			grant, ctx, directory := storageGrantFixture(t)
			serial := make(chan struct{}, 1)
			lifetime, err := acquireStorageSerial(t.Context(), serial)
			if err != nil {
				t.Fatal(err)
			}
			grant.lifetime, lifetime.native = lifetime, true
			name := filepath.Join(directory, "fixture.db")
			if err := os.WriteFile(name, []byte("database"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := grant.open("fixture.db")
			if err != nil {
				t.Fatal(err)
			}
			lock := unix.Flock_t{Type: unix.F_RDLCK, Start: 0x40000002, Len: 510}
			if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock); err != nil {
				t.Fatal(err)
			}
			grant.closeMember(file)
			connection, peer := net.Pipe()
			workerContext, stopWorker := context.WithCancel(t.Context())
			client := &workerClient{stdin: connection, stdout: connection, cancel: stopWorker,
				done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}),
				pending: map[string]*pendingCall{}, slots: make(chan struct{}, 1), regularSlots: make(chan struct{}, 1),
				writes: make(chan workerWrite, 1), cancelFrames: make(chan sdk.Frame, 1), lifetime: workerContext}
			go client.read()
			go client.writeLoop()
			t.Cleanup(func() {
				client.fail(io.EOF)
				peer.Close()
				<-client.readDone
				<-client.writeDone
				client.cancelWG.Wait()
				endStorageCall(ctx)
			})
			canceled, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := client.call(canceled, sdk.MethodStorageRead, sdk.Scope{}, nil, struct{}{}, func(event Event) error {
					if event.Name != "fixture.ready" {
						return errors.New("unexpected fixture event")
					}
					cancel()
					return context.Canceled
				})
				done <- err
			}()
			var request sdk.Frame
			decoder := json.NewDecoder(peer)
			if err := decoder.Decode(&request); err != nil || request.Kind != "call" {
				t.Fatal("wire did not receive the owned parent", request, err)
			}
			if err := json.NewEncoder(peer).Encode(sdk.Frame{Kind: "event", ID: request.ID,
				Seq: 1, Event: "fixture.ready", Data: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("parent did not observe cancellation", err)
			}
			var cancellation sdk.Frame
			if err := decoder.Decode(&cancellation); err != nil || cancellation.Kind != "cancel" {
				t.Fatal("worker did not receive cancellation", cancellation, err)
			}
			if err := grant.Close(); err != nil {
				t.Fatal(err)
			}
			if len(serial) != 1 || storageExternalLockAvailable(t, name) {
				t.Fatal("wire cancellation released locks before a terminal frame")
			}
			reply := sdk.Frame{Kind: "result", ID: request.ID, Result: json.RawMessage(`{}`)}
			if !valid {
				reply.Scope.Session = "foreign"
			}
			if err := json.NewEncoder(peer).Encode(reply); err != nil {
				t.Fatal(err)
			}
			client.cancelWG.Wait()
			if valid && (len(serial) != 0 || !storageExternalLockAvailable(t, name)) {
				t.Fatal("valid wire terminal did not release canceled native admission")
			}
			if !valid && (len(serial) != 1 || storageExternalLockAvailable(t, name)) {
				t.Fatal("invalid wire terminal released uncertain native locks")
			}
		})
	}
}

func TestStorageSerialAdmissionAndDescriptorBudgetFailClosed(t *testing.T) {
	grant, _, _ := storageGrantFixture(t)
	serial := make(chan struct{}, 1)
	lifetime, err := acquireStorageSerial(t.Context(), serial)
	if err != nil {
		t.Fatal(err)
	}
	grant.lifetime = lifetime
	lifetime.native = true
	defer lifetime.close()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := acquireStorageSerial(canceled, serial); !errors.Is(err, context.Canceled) {
		t.Fatal("overlapping storage lifetime ignored cancellation", err)
	}
	for range storageMaxHeldDescriptors {
		file, err := grant.open("legacy")
		if err != nil {
			t.Fatal(err)
		}
		grant.closeMember(file)
	}
	if file, err := grant.open("legacy"); err == nil {
		file.Close()
		t.Fatal("native descriptor retention grew beyond its finite budget")
	}
}
