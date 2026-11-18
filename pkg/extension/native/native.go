// Package native registers an SDK module in a Go C-shared library.
package native

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

var ErrUnavailable = errors.New("native extension authoring requires cgo")

var (
	errRegistered = errors.New("native extension module is already registered")
	errModule     = errors.New("native extension requires a registered native module")
	errSession    = errors.New("unknown or closed native extension session")
	errFrame      = errors.New("native extension byte span exceeds its frame limit")
	errCallback   = errors.New("native extension host callback failed")
	library       = newLibrary()
)

// Register validates and snapshots one module per library. Call it from init:
// main is not called when the host loads a C-shared library. Handlers and
// inbound implementations must support concurrent calls and sessions.
func Register(module sdk.Module) error {
	if !available {
		return ErrUnavailable
	}
	return library.register(module)
}

type nativeLibrary struct {
	mu       sync.Mutex
	module   *sdk.Module
	next     uint64
	sessions map[uint64]*session
}

func newLibrary() *nativeLibrary {
	return &nativeLibrary{sessions: make(map[uint64]*session)}
}

func (lib *nativeLibrary) register(module sdk.Module) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("native extension module validation failed")
		}
	}()
	if module.Manifest.Runtime != "native" {
		return errModule
	}
	snapshot := sdk.Module{
		Manifest: module.Manifest.Clone(),
		Handlers: maps.Clone(module.Handlers),
		Inbounds: slices.Clone(module.Inbounds),
		Actions:  maps.Clone(module.Actions),
		Start:    module.Start,
	}
	if err := snapshot.Validate(); err != nil {
		if len(err.Error()) > 4096 {
			return errors.New("native extension module validation failed")
		}
		return err
	}
	lib.mu.Lock()
	defer lib.mu.Unlock()
	if lib.module != nil {
		return errRegistered
	}
	lib.module = &snapshot
	return nil
}

func (lib *nativeLibrary) open(emit func([]byte) error) (uint64, error) {
	lib.mu.Lock()
	defer lib.mu.Unlock()
	if lib.module == nil || emit == nil {
		return 0, errModule
	}
	if lib.next == ^uint64(0) {
		return 0, errors.New("native extension session handles exhausted")
	}
	lib.next++
	id := lib.next
	ctx, cancel := context.WithCancel(context.Background())
	reader, input := io.Pipe()
	state := &session{
		ctx: ctx, cancel: cancel, reader: reader, input: input, emit: emit,
		started: make(chan struct{}), done: make(chan struct{}), sendGate: make(chan struct{}, 1),
	}
	lib.sessions[id] = state
	go func(module sdk.Module) {
		defer func() {
			recover()
			state.stop()
			lib.mu.Lock()
			delete(lib.sessions, id)
			lib.mu.Unlock()
			close(state.done)
		}()
		select {
		case <-state.ctx.Done():
			return
		case <-state.started:
		}
		_ = sdk.Serve(state.ctx, reader, state, module)
	}(*lib.module)
	return id, nil
}

func (lib *nativeLibrary) get(id uint64) *session {
	lib.mu.Lock()
	defer lib.mu.Unlock()
	return lib.sessions[id]
}

func (lib *nativeLibrary) send(id uint64, data []byte) error {
	if len(data) > sdk.MaxFrameBytes+1 {
		return errFrame
	}
	state := lib.get(id)
	if state == nil {
		return errSession
	}
	return state.send(data)
}

func (lib *nativeLibrary) close(id uint64) error {
	lib.mu.Lock()
	state := lib.sessions[id]
	delete(lib.sessions, id)
	lib.mu.Unlock()
	if state == nil {
		return errSession
	}
	state.stop()
	return nil
}

type session struct {
	ctx       context.Context
	cancel    context.CancelFunc
	reader    *io.PipeReader
	input     *io.PipeWriter
	emit      func([]byte) error
	started   chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	sendGate  chan struct{}
	callback  sync.Mutex
	revoked   bool
}

func (state *session) send(data []byte) error {
	select {
	case <-state.ctx.Done():
		return errSession
	case state.sendGate <- struct{}{}:
	}
	defer func() { <-state.sendGate }()
	if state.ctx.Err() != nil {
		return errSession
	}
	body := bytes.Clone(data)
	state.startOnce.Do(func() { close(state.started) })
	_, err := state.input.Write(body)
	return err
}

func (state *session) Write(data []byte) (int, error) {
	if len(data) > sdk.MaxFrameBytes+1 {
		return 0, errFrame
	}
	body := bytes.Clone(data)
	written := make(chan error, 1)
	state.callback.Lock()
	if state.revoked {
		state.callback.Unlock()
		return 0, errSession
	}
	// Reserve the in-flight callback before Close can revoke the session.
	// Its borrowed buffer stays alive if a trusted C callback blocks.
	go func() {
		var err error
		defer func() {
			if recover() != nil {
				err = errCallback
			}
			written <- err
		}()
		err = state.emit(body)
	}()
	state.callback.Unlock()
	select {
	case err := <-written:
		if err != nil {
			return 0, errCallback
		}
		return len(data), nil
	case <-state.ctx.Done():
		return 0, errSession
	}
}

func (state *session) Close() error {
	state.stop()
	return nil
}

func (state *session) stop() {
	state.stopOnce.Do(func() {
		state.callback.Lock()
		state.revoked = true
		state.callback.Unlock()
		state.cancel()
		state.input.CloseWithError(errSession)
		state.reader.CloseWithError(errSession)
	})
}
