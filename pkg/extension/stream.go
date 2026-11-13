package extension

import (
	"context"
	"encoding/json"
	"strconv"
)

type Stream struct {
	server       *worker
	state        *callState
	emitGate     chan struct{}
	hostGate     chan struct{}
	seq          uint64
	actionEffect string
}

func (stream *Stream) Emit(ctx context.Context, event string, data any) (err error) {
	if effect, hook := GatewayMethodEffect(stream.state.call.Method); hook && (effect != "write" || event != "accepted") {
		err := &Error{Code: "invalid_params", Message: "gateway hooks are unary and permit only write acceptance receipts"}
		stream.server.mu.Lock()
		stream.state.creditErr = err
		stream.server.mu.Unlock()
		return err
	}
	if stream.state.call.Method == MethodModuleCall && (stream.actionEffect != "write" || event != "accepted") {
		err := &Error{Code: "invalid_params", Message: "module.call is unary and permits only a write acceptance receipt"}
		stream.server.mu.Lock()
		stream.state.creditErr = err
		stream.server.mu.Unlock()
		return err
	}
	if ctx == nil || event == "" || len(event) > 128 {
		return &Error{Code: "invalid_params", Message: "invalid extension event"}
	}
	ctx, cancel := stream.context(ctx)
	defer cancel()
	if err := stream.take(ctx, stream.emitGate); err != nil {
		return err
	}
	defer func() { <-stream.emitGate }()
	payload, err := marshalPayload(data)
	if err != nil {
		return err
	}
	if event == "data" {
		var chunk []byte
		if json.Unmarshal(payload, &chunk) != nil || len(chunk) > MaxChunkBytes {
			return &Error{Code: "resource_exhausted", Message: "extension data chunk exceeds its byte limit or is not base64"}
		}
	} else if len(payload) > MaxChunkBytes {
		return &Error{Code: "resource_exhausted", Message: "extension event exceeds its byte limit"}
	}
	if event == "accepted" && (len(payload) == 0 || payload[0] != '{') {
		return &Error{Code: "invalid_params", Message: "acceptance event requires a JSON object"}
	}
	stream.server.mu.Lock()
	if stream.state.finished || stream.state.ctx.Err() != nil {
		stream.server.mu.Unlock()
		return context.Canceled
	}
	if stream.state.creditErr != nil {
		err := stream.state.creditErr
		stream.server.mu.Unlock()
		return err
	}
	stream.seq++
	if stream.seq == 0 {
		stream.server.mu.Unlock()
		return &Error{Code: "resource_exhausted", Message: "extension event sequence exhausted"}
	}
	waiting := &ackWait{seq: stream.seq, done: make(chan struct{})}
	stream.state.pending = waiting
	stream.server.mu.Unlock()
	defer func() {
		stream.server.mu.Lock()
		if stream.state.pending == waiting {
			if err != nil {
				stream.state.creditErr = err
			} else {
				stream.state.pending = nil
			}
		}
		stream.server.mu.Unlock()
	}()
	if err := stream.server.send(ctx, Frame{
		Kind: KindEvent, ID: stream.state.call.ID, Seq: waiting.seq, Event: event, Data: payload,
	}); err != nil {
		return err
	}
	select {
	case <-waiting.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.state.ctx.Done():
		return stream.state.ctx.Err()
	case <-stream.server.ctx.Done():
		return context.Cause(stream.server.ctx)
	}
}

func (stream *Stream) HostCall(ctx context.Context, method string, params any, out any) error {
	if ctx == nil || method != MethodStateGet && method != MethodStateCAS && method != MethodNetworkProxy && method != MethodAccountReplace && method != MethodComponentCall {
		return &Error{Code: "method_not_found", Message: "host service method is not supported"}
	}
	ctx, cancel := stream.context(ctx)
	defer cancel()
	if err := stream.take(ctx, stream.hostGate); err != nil {
		return err
	}
	defer func() { <-stream.hostGate }()
	payload, err := marshalPayload(params)
	if err != nil {
		return err
	}
	if len(payload) > MaxHostBytes {
		return &Error{Code: "resource_exhausted", Message: "host service parameters exceed their byte limit"}
	}
	stream.server.mu.Lock()
	if stream.state.finished || stream.state.ctx.Err() != nil {
		stream.server.mu.Unlock()
		return context.Canceled
	}
	var id string
	for {
		stream.server.nextHost++
		id = "host/" + strconv.FormatUint(stream.server.nextHost, 10)
		if stream.server.calls[id] == nil && stream.server.host[id] == nil && stream.server.closedHosts.byID[id] == nil {
			break
		}
	}
	waiting := &hostWait{parent: stream.state, done: make(chan Frame, 1)}
	stream.server.host[id] = waiting
	stream.server.mu.Unlock()
	defer func() {
		stream.server.mu.Lock()
		if stream.server.host[id] == waiting {
			stream.server.closedHosts.remember(id, 0, waiting.parent.call.ID)
			delete(stream.server.host, id)
		}
		stream.server.mu.Unlock()
	}()
	frame := Frame{
		Kind: KindHostCall, ID: id, Parent: stream.state.call.ID, Method: method,
		Params: payload, Scope: stream.state.call.Scope,
	}
	if deadline, ok := ctx.Deadline(); ok {
		frame.Deadline = deadline.UnixMilli()
	}
	if err := stream.server.send(ctx, frame); err != nil {
		return err
	}
	select {
	case reply := <-waiting.done:
		if reply.Error != nil {
			return reply.Error
		}
		if len(reply.Result) > MaxHostBytes {
			return &Error{Code: "resource_exhausted", Message: "host service result exceeds its byte limit"}
		}
		if out != nil {
			if err := json.Unmarshal(reply.Result, out); err != nil {
				return &Error{Code: "protocol_error", Message: "invalid host service result"}
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.state.ctx.Done():
		return stream.state.ctx.Err()
	case <-stream.server.ctx.Done():
		return context.Cause(stream.server.ctx)
	}
}

func (stream *Stream) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := stream.state.ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		stop := context.AfterFunc(stream.state.ctx, cancel)
		return ctx, func() { stop(); cancel() }
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(stream.state.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (stream *Stream) take(ctx context.Context, gate chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case gate <- struct{}{}:
		select {
		case <-stream.state.ctx.Done():
			<-gate
			return stream.state.ctx.Err()
		default:
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.state.ctx.Done():
		return stream.state.ctx.Err()
	case <-stream.server.ctx.Done():
		return context.Cause(stream.server.ctx)
	}
}
