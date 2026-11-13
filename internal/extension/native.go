//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type nativeLibrary struct {
	open  func(uintptr) (uint64, error)
	send  func(uint64, []byte) error
	close func(uint64) error
}

type nativeWrite struct {
	data []byte
	done chan error
}

type nativeBridge struct {
	mu       sync.Mutex
	id       uintptr
	image    *nativeLibrary
	session  uint64
	stopped  bool
	err      error
	stopOnce sync.Once
	done     chan struct{}
	reader   *io.PipeReader
	writer   *io.PipeWriter
	writes   chan nativeWrite
	sendGate chan struct{}
	emitGate chan struct{}
}

var nativeHosts = struct {
	sync.Mutex
	next    uintptr
	bridges map[uintptr]*nativeBridge
}{bridges: make(map[uintptr]*nativeBridge)}

var nativeOpenSlots = make(chan struct{}, sdk.MaxConcurrent)

func openNativeTransport(ctx context.Context, filename string) (*moduleTransport, error) {
	return openNativeTransportWith(ctx, filename, loadNativeLibrary)
}

func openNativeTransportWith(ctx context.Context, filename string, load func(string) (*nativeLibrary, error)) (*moduleTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filename == "" || strings.ContainsRune(filename, 0) {
		return nil, errors.New("native extension requires a valid library filename")
	}
	filename, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	select {
	case nativeOpenSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	reader, writer := io.Pipe()
	bridge := &nativeBridge{
		reader: reader, writer: writer, done: make(chan struct{}), writes: make(chan nativeWrite),
		sendGate: make(chan struct{}, 1), emitGate: make(chan struct{}, 1),
	}
	nativeHosts.Lock()
	if nativeHosts.next == ^uintptr(0) {
		nativeHosts.Unlock()
		<-nativeOpenSlots
		reader.Close()
		writer.Close()
		return nil, errors.New("native extension host handles exhausted")
	}
	nativeHosts.next++
	bridge.id = nativeHosts.next
	nativeHosts.bridges[bridge.id] = bridge
	nativeHosts.Unlock()
	opened := make(chan error, 1)
	go func() {
		defer func() { <-nativeOpenSlots }()
		image, err := load(filename)
		if err != nil {
			bridge.stop(err)
			opened <- err
			return
		}
		select {
		case <-bridge.done:
			opened <- bridge.failure()
			return
		default:
		}
		session, err := image.open(bridge.id)
		if err != nil {
			bridge.stop(err)
			if session != 0 {
				go image.close(session)
			}
			opened <- err
			return
		}
		bridge.mu.Lock()
		if bridge.stopped {
			bridge.mu.Unlock()
			go image.close(session)
			opened <- io.ErrClosedPipe
			return
		}
		bridge.image, bridge.session = image, session
		bridge.mu.Unlock()
		go bridge.sendLoop(image, session)
		opened <- nil
	}()
	select {
	case err := <-opened:
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			bridge.stop(err)
			return nil, err
		}
		return &moduleTransport{
			input: nativeInput{bridge}, output: nativeOutput{bridge},
			diagnostics: io.NopCloser(bytes.NewReader(nil)),
			stop:        func() { bridge.stop(nil) },
			wait: func() error {
				<-bridge.done
				bridge.mu.Lock()
				defer bridge.mu.Unlock()
				return bridge.err
			},
		}, nil
	case <-ctx.Done():
		bridge.stop(ctx.Err())
		return nil, ctx.Err()
	}
}

func (bridge *nativeBridge) sendLoop(image *nativeLibrary, session uint64) {
	for {
		select {
		case <-bridge.done:
			return
		case work := <-bridge.writes:
			select {
			case <-bridge.done:
				work.done <- bridge.failure()
				return
			default:
			}
			err := image.send(session, work.data)
			if err != nil {
				bridge.stop(err)
			}
			work.done <- err
			if err != nil {
				return
			}
		}
	}
}

// stop retires host authority and byte I/O without waiting for arbitrary C.
// A stuck open/send/close can retain its goroutine and mapped library, but
// cannot keep Manager.Close waiting or use a revoked host callback ID.
func (bridge *nativeBridge) stop(err error) {
	bridge.stopOnce.Do(func() {
		bridge.mu.Lock()
		bridge.stopped, bridge.err = true, err
		image, session := bridge.image, bridge.session
		bridge.mu.Unlock()
		nativeHosts.Lock()
		delete(nativeHosts.bridges, bridge.id)
		nativeHosts.Unlock()
		close(bridge.done)
		pipeErr := err
		if pipeErr == nil {
			pipeErr = io.ErrClosedPipe
		}
		bridge.writer.CloseWithError(pipeErr)
		bridge.reader.CloseWithError(pipeErr)
		if image != nil {
			go image.close(session)
		}
	})
}

func (bridge *nativeBridge) failure() error {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.err != nil {
		return bridge.err
	}
	return io.ErrClosedPipe
}

type nativeInput struct{ bridge *nativeBridge }

func (input nativeInput) Write(data []byte) (int, error) {
	bridge := input.bridge
	if len(data) > sdk.MaxFrameBytes+1 {
		return 0, errors.New("native extension input exceeds its frame limit")
	}
	select {
	case <-bridge.done:
		return 0, bridge.failure()
	case bridge.sendGate <- struct{}{}:
	}
	defer func() { <-bridge.sendGate }()
	select {
	case <-bridge.done:
		return 0, bridge.failure()
	default:
	}
	if len(data) == 0 {
		return 0, nil
	}
	work := nativeWrite{data: bytes.Clone(data), done: make(chan error, 1)}
	select {
	case <-bridge.done:
		return 0, bridge.failure()
	case bridge.writes <- work:
	}
	select {
	case <-bridge.done:
		return 0, bridge.failure()
	case err := <-work.done:
		if err != nil {
			return 0, err
		}
		return len(data), nil
	}
}

func (input nativeInput) Close() error {
	input.bridge.stop(nil)
	return nil
}

type nativeOutput struct{ bridge *nativeBridge }

func (output nativeOutput) Read(data []byte) (int, error) {
	return output.bridge.reader.Read(data)
}

func (output nativeOutput) Close() error {
	output.bridge.stop(nil)
	return nil
}

func nativeEmit(host uintptr, length uint64, copyBytes func() []byte) error {
	if length > sdk.MaxFrameBytes+1 {
		return errors.New("native extension output exceeds its frame limit")
	}
	nativeHosts.Lock()
	bridge := nativeHosts.bridges[host]
	nativeHosts.Unlock()
	if bridge == nil {
		return errors.New("native extension host handle is revoked")
	}
	select {
	case <-bridge.done:
		return bridge.failure()
	case bridge.emitGate <- struct{}{}:
	}
	defer func() { <-bridge.emitGate }()
	select {
	case <-bridge.done:
		return bridge.failure()
	default:
	}
	data := copyBytes()
	if uint64(len(data)) != length {
		return fmt.Errorf("native extension output length changed")
	}
	_, err := bridge.writer.Write(data)
	if err != nil {
		bridge.stop(err)
	}
	return err
}
