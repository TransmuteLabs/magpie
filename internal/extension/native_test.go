//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func nativeTestReceive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("native fixture did not reach its expected state")
		var zero T
		return zero
	}
}

func TestNativeTransportKeepsServingAfterOpenContextEnds(t *testing.T) {
	var host uintptr
	closed := make(chan uint64, 1)
	image := &nativeLibrary{
		open: func(id uintptr) (uint64, error) { host = id; return 42, nil },
		send: func(session uint64, data []byte) error {
			if session != 42 {
				return errors.New("wrong native session")
			}
			return nativeEmit(host, uint64(len(data)), func() []byte { return bytes.Clone(data) })
		},
		close: func(session uint64) error { closed <- session; return nil },
	}
	ctx, cancel := context.WithCancel(t.Context())
	transport, err := openNativeTransportWith(ctx, "fixture", func(string) (*nativeLibrary, error) { return image, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer transport.stop()
	cancel()
	written := make(chan error, 1)
	body := []byte("{\"kind\":\"result\"}\n")
	go func() {
		count, err := transport.input.Write(body)
		if count != len(body) && err == nil {
			err = io.ErrShortWrite
		}
		written <- err
	}()
	got := make([]byte, len(body))
	if _, err := io.ReadFull(transport.output, got); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("native transport changed its stream: %q, %v", got, err)
	}
	if err := nativeTestReceive(t, written); err != nil {
		t.Fatal(err)
	}
	if diagnostics, err := io.ReadAll(transport.diagnostics); err != nil || len(diagnostics) != 0 {
		t.Fatalf("native transport redirected process diagnostics: %q, %v", diagnostics, err)
	}
	var group sync.WaitGroup
	for range 16 {
		group.Go(transport.stop)
	}
	group.Wait()
	if err := transport.wait(); err != nil {
		t.Fatal(err)
	}
	if session := nativeTestReceive(t, closed); session != 42 {
		t.Fatalf("closed session %d, want 42", session)
	}
	if err := nativeEmit(host, 1, func() []byte {
		t.Error("revoked native callback copied a borrowed pointer")
		return []byte("x")
	}); err == nil {
		t.Fatal("late native callback retained host authority")
	}
}

func TestNativeStopUnblocksBorrowedEmit(t *testing.T) {
	var host uintptr
	copying := make(chan struct{})
	finished := make(chan error, 1)
	image := &nativeLibrary{
		open: func(id uintptr) (uint64, error) { host = id; return 1, nil },
		send: func(uint64, []byte) error {
			err := nativeEmit(host, 1, func() []byte {
				close(copying)
				return []byte("x")
			})
			finished <- err
			return err
		},
		close: func(uint64) error { return nil },
	}
	transport, err := openNativeTransportWith(t.Context(), "fixture", func(string) (*nativeLibrary, error) { return image, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer transport.stop()
	written := make(chan error, 1)
	go func() { _, err := transport.input.Write([]byte("input\n")); written <- err }()
	nativeTestReceive(t, copying)
	transport.stop()
	if err := nativeTestReceive(t, written); err == nil {
		t.Fatal("stop left a blocked native write successful")
	}
	if err := nativeTestReceive(t, finished); err == nil {
		t.Fatal("stop did not release an in-flight native emit callback")
	}
	if _, err := transport.output.Read(make([]byte, 1)); err == nil {
		t.Fatal("stop left native output open")
	}
	if err := nativeEmit(host, 0, func() []byte { return nil }); err == nil {
		t.Fatal("closed native session accepted a late callback")
	}
}

func TestNativeStopDoesNotWaitForUncooperativeCode(t *testing.T) {
	sending, closing := make(chan struct{}), make(chan struct{})
	releaseSend, releaseClose := make(chan struct{}), make(chan struct{})
	sendReturned, closeReturned := make(chan struct{}), make(chan struct{})
	image := &nativeLibrary{
		open: func(uintptr) (uint64, error) { return 1, nil },
		send: func(uint64, []byte) error {
			close(sending)
			<-releaseSend
			close(sendReturned)
			return nil
		},
		close: func(uint64) error {
			close(closing)
			<-releaseClose
			close(closeReturned)
			return nil
		},
	}
	transport, err := openNativeTransportWith(t.Context(), "fixture", func(string) (*nativeLibrary, error) { return image, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(releaseSend)
		close(releaseClose)
		nativeTestReceive(t, sendReturned)
		nativeTestReceive(t, closeReturned)
	}()
	written := make(chan error, 1)
	go func() { _, err := transport.input.Write([]byte("input\n")); written <- err }()
	nativeTestReceive(t, sending)
	stopped := make(chan error, 1)
	go func() { transport.stop(); stopped <- transport.wait() }()
	if err := nativeTestReceive(t, stopped); err != nil {
		t.Fatal(err)
	}
	nativeTestReceive(t, closing)
	if err := nativeTestReceive(t, written); err == nil {
		t.Fatal("uncooperative native send kept the host writer alive")
	}
}

func TestNativeOpenCancellationRevokesLateSession(t *testing.T) {
	entered := make(chan uintptr, 1)
	release := make(chan struct{})
	closed := make(chan uint64, 1)
	image := &nativeLibrary{
		open: func(host uintptr) (uint64, error) {
			entered <- host
			<-release
			return 73, nil
		},
		send:  func(uint64, []byte) error { return nil },
		close: func(session uint64) error { closed <- session; return nil },
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		transport, err := openNativeTransportWith(ctx, "fixture", func(string) (*nativeLibrary, error) { return image, nil })
		if transport != nil {
			transport.stop()
		}
		result <- err
	}()
	host := nativeTestReceive(t, entered)
	cancel()
	if err := nativeTestReceive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("native open ignored its deadline: %v", err)
	}
	if err := nativeEmit(host, 1, func() []byte {
		t.Error("timed-out native open retained its callback authority")
		return []byte("x")
	}); err == nil {
		t.Fatal("native open cancellation did not revoke callbacks")
	}
	close(release)
	if session := nativeTestReceive(t, closed); session != 73 {
		t.Fatalf("late native session was not closed: %d", session)
	}
}

func TestNativeLoadCancellationDoesNotOpenSession(t *testing.T) {
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := openNativeTransportWith(ctx, "fixture", func(string) (*nativeLibrary, error) {
			close(entered)
			<-release
			defer close(returned)
			return &nativeLibrary{open: func(uintptr) (uint64, error) {
				t.Error("canceled library load opened a new native session")
				return 0, errors.New("unexpected native open")
			}}, nil
		})
		result <- err
	}()
	nativeTestReceive(t, entered)
	cancel()
	if err := nativeTestReceive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("stuck library constructor blocked cancellation: %v", err)
	}
	close(release)
	nativeTestReceive(t, returned)
}

func TestNativeRejectsOversizedBytesBeforeCopy(t *testing.T) {
	var host uintptr
	image := &nativeLibrary{
		open:  func(id uintptr) (uint64, error) { host = id; return 1, nil },
		send:  func(uint64, []byte) error { t.Error("oversized bytes reached native code"); return nil },
		close: func(uint64) error { return nil },
	}
	transport, err := openNativeTransportWith(t.Context(), "fixture", func(string) (*nativeLibrary, error) { return image, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer transport.stop()
	if err := nativeEmit(host, sdk.MaxFrameBytes+2, func() []byte {
		t.Error("oversized native callback copied a borrowed pointer")
		return nil
	}); err == nil {
		t.Fatal("oversized native output accepted")
	}
	if _, err := transport.input.Write(make([]byte, sdk.MaxFrameBytes+2)); err == nil {
		t.Fatal("oversized native input accepted")
	}
}

func TestNativeMissingLibraryAndInvalidFilename(t *testing.T) {
	for _, filename := range []string{"", "invalid\x00library", filepath.Join(t.TempDir(), "missing-library")} {
		transport, err := openNativeTransport(t.Context(), filename)
		if err == nil || transport != nil {
			t.Fatalf("invalid native library accepted: %q, %v", filename, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := openNativeTransport(ctx, "fixture"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled native open did work: %v", err)
	}
	if _, err := loadNativeLibrary(filepath.Join(t.TempDir(), "missing-library")); err == nil || !strings.Contains(err.Error(), "load native library") {
		t.Fatalf("loader failure lost its context: %v", err)
	}
}
