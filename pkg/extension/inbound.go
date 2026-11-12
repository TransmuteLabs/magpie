package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yetone/magpie/pkg/protocolext"
)

const (
	ResponseStart  = "start"
	ResponseWrite  = "write"
	ResponseFlush  = "flush"
	ResponseFinish = "finish"
	ResponseCancel = "cancel"
)

type inboundModule struct {
	adapters     map[string]protocolext.Inbound
	discovery    bool
	toolProvider bool
	tools        map[string]bool
	mu           sync.Mutex
	responses    map[string]*inboundResponse
}

type inboundResponse struct {
	id       string
	adapter  string
	scope    Scope
	ctx      context.Context
	cancel   context.CancelFunc
	gate     chan struct{}
	response protocolext.Response
	codec    protocolext.Inbound
	writer   *nativeWriter
	closed   bool
}

type nativeWriter struct {
	headers http.Header
	stream  *Stream
	ctx     context.Context
	started bool
	err     error
}

func newInboundModule(manifest Manifest, adapters []protocolext.Inbound) (*inboundModule, error) {
	registry, err := protocolext.New(adapters...)
	if err != nil {
		return nil, err
	}
	if len(registry.Paths()) != len(manifest.Inbounds) {
		return nil, fmt.Errorf("inbound helpers do not match manifest descriptors")
	}
	module := &inboundModule{
		adapters: make(map[string]protocolext.Inbound), responses: make(map[string]*inboundResponse),
		discovery:    manifest.Contract.Has(ModelDiscovery),
		toolProvider: manifest.Contract.Has(ToolProvider), tools: make(map[string]bool),
	}
	for _, tool := range manifest.Tools {
		module.tools[tool.Name] = true
	}
	for _, descriptor := range manifest.Inbounds {
		adapter, found := registry.Lookup(descriptor.Path)
		if !found || adapter.ID() != descriptor.ID {
			return nil, fmt.Errorf("inbound helper does not match descriptor %q", descriptor.ID)
		}
		module.adapters[descriptor.ID] = adapter
	}
	return module, nil
}

func (module *inboundModule) decode(ctx context.Context, call Call, stream *Stream) (any, error) {
	var request NativeRequest
	if err := decodeParams(call.Params, &request); err != nil {
		return nil, err
	}
	if len(request.Body) > MaxNativeBodyBytes {
		return nil, &Error{Code: "resource_exhausted", Message: "native request body exceeds its byte limit"}
	}
	record, err := module.acquire(ctx, call, stream, request.ID, request.ResponseID, true)
	if err != nil {
		return nil, err
	}
	defer record.release()
	adapter := record.codec
	if adapter == nil {
		return nil, &Error{Code: "internal", Message: "inbound binder returned no codec"}
	}
	decoded, err := adapter.Decode(ctx, request.Headers.Clone(), request.Body)
	if err != nil {
		var public *protocolext.Error
		if errors.As(err, &public) && public != nil {
			return nil, &Error{Code: "invalid_params", Message: public.Message}
		}
		return nil, err
	}
	if record.writer.err != nil {
		return nil, record.writer.err
	}
	if decoded.Timeout < 0 || len(decoded.Body) > MaxNativeBodyBytes {
		return nil, &Error{Code: "internal", Message: "codec produced an invalid request"}
	}
	if len(decoded.Metadata) > MaxChunkBytes || len(decoded.Metadata) != 0 && !json.Valid(decoded.Metadata) {
		return nil, &Error{Code: "internal", Message: "codec produced invalid or oversized metadata"}
	}
	switch string(decoded.Protocol) {
	case "models":
		if !module.discovery || !adapter.Contract().Has(protocolext.ModelDiscovery) || len(decoded.Body) != 0 {
			return nil, &Error{Code: "permission_denied", Message: "codec did not negotiate model discovery"}
		}
	case "tool":
		if !module.toolProvider || !adapter.Contract().Has(protocolext.ToolProvider) {
			return nil, &Error{Code: "permission_denied", Message: "codec did not negotiate tool invocation"}
		}
		var request ToolRequest
		if decodeParams(decoded.Body, &request) != nil || !module.tools[request.ID] || !json.Valid(request.Arguments) {
			return nil, &Error{Code: "invalid_params", Message: "codec produced an invalid or unadvertised tool invocation"}
		}
	case "chat", "responses", "messages", "anthropic", "gemini":
		if !json.Valid(decoded.Body) {
			return nil, &Error{Code: "internal", Message: "codec produced invalid canonical JSON"}
		}
	default:
		return nil, &Error{Code: "internal", Message: "codec produced an unsupported canonical protocol"}
	}
	timeout := decoded.Timeout.Milliseconds()
	if decoded.Timeout%time.Millisecond != 0 {
		timeout++
	}
	return DecodedRequest{
		Protocol: string(decoded.Protocol), Body: decoded.Body, Session: decoded.Session, TimeoutMs: timeout,
		RequestID: decoded.RequestID, Metadata: bytes.Clone(decoded.Metadata),
	}, nil
}

func (module *inboundModule) response(ctx context.Context, call Call, stream *Stream) (any, error) {
	var event ResponseEvent
	if err := decodeParams(call.Params, &event); err != nil {
		return nil, err
	}
	if len(event.Body) > MaxChunkBytes {
		return nil, &Error{Code: "resource_exhausted", Message: "canonical response chunk exceeds its byte limit"}
	}
	switch event.Action {
	case ResponseStart, ResponseWrite, ResponseFlush, ResponseFinish, ResponseCancel:
	default:
		return nil, &Error{Code: "invalid_params", Message: "unknown inbound response action"}
	}
	record, err := module.acquire(ctx, call, stream, event.ID, event.ResponseID, event.Action == ResponseStart)
	if err != nil {
		return nil, err
	}
	defer record.release()
	switch event.Action {
	case ResponseStart:
		for name, values := range event.Headers {
			record.response.Header()[name] = append([]string(nil), values...)
		}
		if event.Status != 0 {
			if event.Status < 100 || event.Status > 599 {
				return nil, &Error{Code: "invalid_params", Message: "invalid canonical response status"}
			}
			record.response.WriteHeader(event.Status)
		}
	case ResponseWrite:
		written, err := record.response.Write(event.Body)
		if err != nil {
			return nil, err
		}
		if written != len(event.Body) {
			return nil, io.ErrShortWrite
		}
	case ResponseFlush:
		if flusher, ok := record.response.(http.Flusher); ok {
			flusher.Flush()
		}
	case ResponseFinish, ResponseCancel:
		module.remove(record)
		record.closed = true
		defer record.cancel()
		var cause error
		if event.Action == ResponseCancel {
			cause = context.Canceled
		} else if event.Error != nil {
			switch event.Error.Code {
			case "canceled":
				cause = context.Canceled
			case "deadline":
				cause = context.DeadlineExceeded
			default:
				cause = event.Error
			}
		}
		if err := record.response.Finish(cause); err != nil {
			return nil, err
		}
	}
	if record.writer.err != nil {
		return nil, record.writer.err
	}
	return struct{}{}, nil
}

func (module *inboundModule) acquire(ctx context.Context, call Call, stream *Stream, adapterID, responseID string, create bool) (*inboundResponse, error) {
	adapter := module.adapters[adapterID]
	if adapter == nil || responseID == "" || len(responseID) > 256 ||
		strings.ContainsFunc(responseID, func(char rune) bool { return unicode.IsControl(char) || unicode.IsSpace(char) }) {
		return nil, &Error{Code: "invalid_params", Message: "invalid inbound adapter or response id"}
	}
	module.prune()
	module.mu.Lock()
	record := module.responses[responseID]
	if record == nil {
		if !create {
			module.mu.Unlock()
			return nil, &Error{Code: "invalid_params", Message: "unknown inbound response id"}
		}
		if len(module.responses) >= MaxConcurrent {
			module.mu.Unlock()
			return nil, &Error{Code: "resource_exhausted", Message: "too many active inbound responses"}
		}
		lifetime, cancel := context.WithCancel(stream.server.ctx)
		if deadline, ok := ctx.Deadline(); ok {
			cancel()
			lifetime, cancel = context.WithDeadline(stream.server.ctx, deadline)
		}
		record = &inboundResponse{
			id: responseID, adapter: adapterID, scope: call.Scope, ctx: lifetime, cancel: cancel, gate: make(chan struct{}, 1),
			writer: &nativeWriter{headers: make(http.Header)},
		}
		module.responses[responseID] = record
	}
	module.mu.Unlock()
	select {
	case record.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if record.closed || record.ctx.Err() != nil {
		<-record.gate
		return nil, &Error{Code: "invalid_params", Message: "inbound response is closed"}
	}
	if record.adapter != adapterID || record.scope != call.Scope {
		<-record.gate
		return nil, &Error{Code: "permission_denied", Message: "inbound response belongs to another scope"}
	}
	record.writer.ctx, record.writer.stream = ctx, stream
	ready := false
	defer func() {
		if !ready {
			module.remove(record)
			record.closed = true
			record.cancel()
			record.release()
		}
	}()
	if record.response == nil {
		bound := protocolext.Bind(adapter, record.ctx)
		if bound != nil {
			record.codec = bound
			record.response = bound.Reply(record.writer)
		}
		if record.response == nil {
			return nil, &Error{Code: "internal", Message: "inbound codec returned no response"}
		}
	}
	ready = true
	return record, nil
}

func (record *inboundResponse) release() {
	record.writer.stream, record.writer.ctx = nil, nil
	<-record.gate
}

func (module *inboundModule) remove(record *inboundResponse) {
	module.mu.Lock()
	if module.responses[record.id] == record {
		delete(module.responses, record.id)
	}
	module.mu.Unlock()
}

func (module *inboundModule) prune() {
	module.mu.Lock()
	var expired []*inboundResponse
	for id, record := range module.responses {
		if record.ctx.Err() != nil {
			delete(module.responses, id)
			expired = append(expired, record)
		}
	}
	module.mu.Unlock()
	for _, record := range expired {
		record.close()
	}
}

func (module *inboundModule) close() {
	module.mu.Lock()
	records := module.responses
	module.responses = make(map[string]*inboundResponse)
	module.mu.Unlock()
	for _, record := range records {
		record.close()
	}
}

func (record *inboundResponse) close() {
	record.cancel()
	record.gate <- struct{}{}
	defer func() { <-record.gate }()
	if !record.closed {
		record.closed = true
		if record.response != nil {
			record.response.Finish(record.ctx.Err())
		}
	}
}

func (writer *nativeWriter) Header() http.Header { return writer.headers }

func (writer *nativeWriter) WriteHeader(status int) {
	if writer.started || writer.err != nil {
		return
	}
	if status < 100 || status > 599 {
		writer.err = &Error{Code: "invalid_params", Message: "codec produced an invalid native status"}
		return
	}
	writer.started = true
	writer.emit("headers", HeadersEvent{Status: status, Headers: writer.headers.Clone()})
}

func (writer *nativeWriter) Write(body []byte) (int, error) {
	if !writer.started {
		writer.WriteHeader(http.StatusOK)
	}
	written := 0
	for len(body) > 0 && writer.err == nil {
		size := min(len(body), MaxChunkBytes)
		writer.emit("data", body[:size])
		if writer.err == nil {
			written += size
			body = body[size:]
		}
	}
	return written, writer.err
}

func (writer *nativeWriter) Flush() {
	if !writer.started {
		writer.WriteHeader(http.StatusOK)
	}
	writer.emit("flush", nil)
}

func (writer *nativeWriter) emit(event string, data any) {
	if writer.err != nil {
		return
	}
	if writer.stream == nil || writer.ctx == nil {
		writer.err = context.Canceled
		return
	}
	writer.err = writer.stream.Emit(writer.ctx, event, data)
}

func decodeParams(params json.RawMessage, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(params))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return &Error{Code: "invalid_params", Message: "invalid extension method parameters"}
	}
	return nil
}
