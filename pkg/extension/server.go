package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
)

type worker struct {
	ctx           context.Context
	cancel        context.CancelCauseFunc
	manifest      Manifest
	handlers      map[string]Handler
	output        chan outputFrame
	mu            sync.Mutex
	calls         map[string]*callState
	host          map[string]*hostWait
	nextHost      uint64
	running       sync.WaitGroup
	closedCredits recentFrames
	closedHosts   recentFrames
	lifetime      *componentLifecycle
}

type callState struct {
	call      Call
	ctx       context.Context
	cancel    context.CancelFunc
	finished  bool
	pending   *ackWait
	creditErr error
}

type ackWait struct {
	seq  uint64
	done chan struct{}
}

type hostWait struct {
	parent *callState
	done   chan Frame
}

type outputFrame struct {
	ctx  context.Context
	body []byte
	done chan error
}

func Serve(ctx context.Context, input io.Reader, output io.Writer, module Module) error {
	if ctx == nil || input == nil || output == nil {
		return fmt.Errorf("extension server requires a context and transport")
	}
	handlers, inbounds, err := module.prepare()
	if err != nil {
		return err
	}
	serverCtx, cancel := context.WithCancelCause(ctx)
	server := &worker{
		ctx: serverCtx, cancel: cancel, manifest: cloneManifest(module.Manifest), handlers: handlers,
		output: make(chan outputFrame, MaxConcurrent), calls: make(map[string]*callState), host: make(map[string]*hostWait),
	}
	server.lifetime = &componentLifecycle{ctx: serverCtx, manifest: cloneManifest(module.Manifest), start: module.Start}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		<-serverCtx.Done()
		if closer, ok := input.(io.Closer); ok {
			closer.Close()
		}
		if closer, ok := output.(io.Closer); ok {
			closer.Close()
		}
	}()
	written := make(chan struct{})
	go func() {
		defer close(written)
		server.writeLoop(output)
	}()
	defer func() {
		cancel(context.Canceled)
		server.lifetime.close()
		server.running.Wait()
		if inbounds != nil {
			inbounds.close()
		}
		<-closed
		<-written
	}()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, MaxChunkBytes), MaxFrameBytes+2)
	for scanner.Scan() {
		if serverCtx.Err() != nil {
			return context.Cause(serverCtx)
		}
		if len(scanner.Bytes()) > MaxFrameBytes {
			return &Error{Code: "protocol_error", Message: "extension frame exceeds its byte limit"}
		}
		var frame Frame
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil {
			return &Error{Code: "protocol_error", Message: "invalid extension frame"}
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return &Error{Code: "protocol_error", Message: "extension frame contains trailing data"}
		}
		if err := server.dispatch(frame); err != nil {
			return err
		}
	}
	if serverCtx.Err() != nil {
		return context.Cause(serverCtx)
	}
	if err := scanner.Err(); err != nil {
		return &Error{Code: "protocol_error", Message: "could not read bounded extension frame"}
	}
	return nil
}

func (server *worker) dispatch(frame Frame) error {
	if frame.ID == "" || len(frame.ID) > 256 || strings.ContainsFunc(frame.ID, func(char rune) bool {
		return unicode.IsControl(char) || unicode.IsSpace(char)
	}) {
		return &Error{Code: "protocol_error", Message: "invalid extension request id"}
	}
	switch frame.Kind {
	case KindCall:
		if frame.Method == "" || frame.Parent != "" || frame.Seq != 0 || frame.Event != "" ||
			frame.Result != nil || frame.Error != nil || frame.Data != nil {
			return &Error{Code: "protocol_error", Message: "invalid call frame"}
		}
		server.mu.Lock()
		if _, exists := server.calls[frame.ID]; exists {
			server.mu.Unlock()
			return &Error{Code: "protocol_error", Message: "duplicate live extension request id"}
		}
		if server.closedCredits.byID[frame.ID] != nil {
			server.mu.Unlock()
			return &Error{Code: "protocol_error", Message: "extension request id still has canceled stream credit"}
		}
		if len(server.calls) >= MaxConcurrent {
			server.mu.Unlock()
			return &Error{Code: "resource_exhausted", Message: "too many concurrent extension calls"}
		}
		ctx, cancel := context.WithCancel(server.ctx)
		if frame.Deadline != 0 {
			cancel()
			ctx, cancel = context.WithDeadline(server.ctx, time.UnixMilli(frame.Deadline))
		}
		state := &callState{
			call: Call{ID: frame.ID, Method: frame.Method, Scope: frame.Scope, Params: frame.Params, Auth: frame.Auth},
			ctx:  ctx, cancel: cancel,
		}
		server.calls[frame.ID] = state
		server.running.Add(1)
		server.mu.Unlock()
		go server.run(state)
	case KindAck:
		if frame.Seq == 0 || frame.Method != "" || frame.Parent != "" || frame.Params != nil ||
			frame.Result != nil || frame.Error != nil || frame.Data != nil || frame.Event != "" || frame.Deadline != 0 {
			return &Error{Code: "protocol_error", Message: "invalid acknowledgement frame"}
		}
		server.mu.Lock()
		state := server.calls[frame.ID]
		if state == nil || state.pending == nil || state.pending.seq != frame.Seq {
			if server.closedCredits.consume(frame.ID, frame.Seq, "") {
				server.mu.Unlock()
				return nil
			}
			server.mu.Unlock()
			return &Error{Code: "protocol_error", Message: "unknown extension acknowledgement sequence"}
		}
		pending := state.pending
		state.pending = nil
		state.creditErr = nil
		close(pending.done)
		server.mu.Unlock()
	case KindCancel:
		server.mu.Lock()
		state := server.calls[frame.ID]
		server.mu.Unlock()
		if state != nil {
			state.cancel()
		}
	case KindHostResult:
		if frame.Method != "" || frame.Seq != 0 || frame.Event != "" || frame.Params != nil || frame.Data != nil ||
			frame.Error != nil && frame.Result != nil {
			return &Error{Code: "protocol_error", Message: "invalid host result frame"}
		}
		server.mu.Lock()
		waiting := server.host[frame.ID]
		if waiting == nil {
			known := server.closedHosts.consume(frame.ID, 0, frame.Parent)
			server.mu.Unlock()
			if known {
				return nil
			}
			return &Error{Code: "protocol_error", Message: "unknown extension host result"}
		}
		if frame.Parent != waiting.parent.call.ID {
			server.mu.Unlock()
			return &Error{Code: "protocol_error", Message: "host result belongs to another parent"}
		}
		delete(server.host, frame.ID)
		waiting.done <- frame
		server.mu.Unlock()
	default:
		return &Error{Code: "protocol_error", Message: "unexpected extension frame kind"}
	}
	return nil
}

func (server *worker) run(state *callState) {
	defer server.running.Done()
	stream := &Stream{
		server: server, state: state, emitGate: make(chan struct{}, 1), hostGate: make(chan struct{}, 1),
	}
	result, err := server.invoke(state, stream)
	if state.ctx.Err() != nil {
		err = state.ctx.Err()
	}
	server.mu.Lock()
	state.finished = true
	if err == nil && state.creditErr != nil {
		err = state.creditErr
	}
	if state.pending != nil {
		server.closedCredits.remember(state.call.ID, state.pending.seq, "")
		state.pending = nil
		if err == nil {
			err = context.Canceled
		}
	}
	server.mu.Unlock()
	state.cancel()
	frame := Frame{Kind: KindResult, ID: state.call.ID}
	if err != nil {
		frame.Error = publicError(err)
	} else {
		frame.Result, err = marshalPayload(result)
		if err != nil {
			frame.Error = publicError(err)
			frame.Result = nil
		}
	}
	if err := server.send(server.ctx, frame); err != nil && server.ctx.Err() == nil {
		if frame.Error == nil {
			fallback := Frame{Kind: KindResult, ID: state.call.ID, Error: publicError(err)}
			if writeError := server.send(server.ctx, fallback); writeError != nil {
				server.cancel(writeError)
			}
		} else {
			server.cancel(err)
		}
	}
	server.mu.Lock()
	delete(server.calls, state.call.ID)
	server.mu.Unlock()
}

func (server *worker) invoke(state *callState, stream *Stream) (result any, err error) {
	defer func() {
		if recover() != nil {
			result, err = nil, &Error{Code: "internal", Message: "extension handler failed"}
		}
	}()
	if err := state.ctx.Err(); err != nil {
		return nil, err
	}
	if state.call.Scope.Plugin != "" && state.call.Scope.Plugin != server.manifest.ID {
		return nil, &Error{Code: "invalid_params", Message: "call scope belongs to another extension"}
	}
	if server.manifest.Contract.Version.Minor >= 1 {
		switch state.call.Method {
		case MethodComponentConfigure:
			return server.lifetime.configure(state.ctx, state.call.Params)
		case MethodComponentReady:
			if len(state.call.Params) != 0 && string(state.call.Params) != "null" {
				if err := decodeParams(state.call.Params, &struct{}{}); err != nil {
					return nil, err
				}
			}
			return server.lifetime.ready()
		case MethodComponentStop:
			if len(state.call.Params) != 0 && string(state.call.Params) != "null" {
				if err := decodeParams(state.call.Params, &struct{}{}); err != nil {
					return nil, err
				}
			}
			return server.lifetime.stop(state.ctx)
		}
	}
	handler := server.handlers[state.call.Method]
	if handler == nil {
		return nil, &Error{Code: "method_not_found", Message: "extension method was not negotiated"}
	}
	ctx := state.ctx
	if server.manifest.Contract.Version.Minor >= 1 && state.call.Method != MethodDescribe {
		var err error
		var stop func()
		ctx, stop, err = server.lifetime.callContext(ctx, state.cancel)
		if err != nil {
			return nil, err
		}
		defer stop()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return handler(ctx, state.call, stream)
}

func (server *worker) send(ctx context.Context, frame Frame) error {
	body, err := marshalPayload(frame)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameBytes {
		return &Error{Code: "resource_exhausted", Message: "extension frame exceeds its byte limit"}
	}
	request := outputFrame{ctx: ctx, body: append(body, '\n'), done: make(chan error, 1)}
	select {
	case server.output <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-server.ctx.Done():
		return context.Cause(server.ctx)
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-server.ctx.Done():
		return context.Cause(server.ctx)
	}
}

func (server *worker) writeLoop(output io.Writer) {
	for {
		select {
		case <-server.ctx.Done():
			return
		case request := <-server.output:
			if err := request.ctx.Err(); err != nil {
				request.done <- err
				continue
			}
			body := request.body
			var err error
			for len(body) > 0 {
				var written int
				written, err = output.Write(body)
				if written < 0 || written > len(body) {
					err = io.ErrShortWrite
					break
				}
				body = body[written:]
				if err != nil {
					break
				}
				if written == 0 {
					err = io.ErrShortWrite
					break
				}
			}
			if err == nil {
				if flusher, ok := output.(interface{ Flush() error }); ok {
					err = flusher.Flush()
				}
			}
			request.done <- err
			if err != nil {
				server.cancel(err)
				return
			}
		}
	}
}

func marshalPayload(value any) (body []byte, err error) {
	defer func() {
		if recover() != nil {
			body, err = nil, &Error{Code: "internal", Message: "could not encode extension payload"}
		}
	}()
	body, err = json.Marshal(value)
	if err != nil {
		return nil, &Error{Code: "internal", Message: "could not encode extension payload"}
	}
	return body, nil
}

func publicError(err error) *Error {
	if errors.Is(err, context.Canceled) {
		return &Error{Code: "canceled", Message: "extension call canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "deadline", Message: "extension call deadline exceeded"}
	}
	var public *Error
	if errors.As(err, &public) && public != nil {
		if !validPublicToken(public.Code) || !validPublicMessage(public.Message) ||
			public.Outcome != "" && !validPublicToken(public.Outcome) {
			return &Error{Code: "internal", Message: "extension handler failed"}
		}
		return &Error{Code: public.Code, Message: public.Message, Outcome: public.Outcome}
	}
	return &Error{Code: "internal", Message: "extension handler failed"}
}
