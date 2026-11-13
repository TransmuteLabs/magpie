package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

type RequestMetadata struct {
	Plugin    string
	RequestID string
	CallerID  string
	Value     json.RawMessage
}

type requestMetadataKey struct{}

func WithRequestMetadata(ctx context.Context, metadata RequestMetadata) context.Context {
	metadata.Value = bytes.Clone(metadata.Value)
	return context.WithValue(ctx, requestMetadataKey{}, metadata)
}

func MetadataFrom(ctx context.Context) RequestMetadata {
	value, _ := ctx.Value(requestMetadataKey{}).(RequestMetadata)
	value.Value = bytes.Clone(value.Value)
	return value
}

func (m *Manager) Protocols() (*codec.Registry, error) {
	entries, err := m.List()
	if err != nil {
		return nil, err
	}
	var adapters []codec.Inbound
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		for _, inbound := range entry.Manifest.Inbounds {
			adapters = append(adapters, &remoteInbound{
				manager: m, module: entry.Manifest.ID, revision: entry.Revision,
				descriptor: inbound, discovery: entry.Manifest.Contract.Has(sdk.ModelDiscovery),
				tools: entry.Manifest.Contract.Has(sdk.ToolProvider),
			})
		}
	}
	return codec.New(adapters...)
}

type remoteInbound struct {
	manager    *Manager
	module     string
	revision   string
	descriptor sdk.InboundDescriptor
	discovery  bool
	tools      bool
}

func (remote *remoteInbound) ID() string   { return remote.module + ":" + remote.descriptor.ID }
func (remote *remoteInbound) Path() string { return remote.descriptor.Path }
func (remote *remoteInbound) Contract() codec.Contract {
	contract := codec.InboundContract()
	if remote.discovery {
		contract = codec.CatalogContract()
	}
	if remote.tools {
		contract.Version = codec.HostContract().Version
		contract.Capabilities = append(contract.Capabilities, codec.ToolProvider)
	}
	return contract
}
func (remote *remoteInbound) Decode(context.Context, http.Header, []byte) (codec.Request, error) {
	return codec.Request{}, errors.New("external codec requires a request binding")
}
func (remote *remoteInbound) Reply(http.ResponseWriter) codec.Response { return nil }

func (remote *remoteInbound) Bind(ctx context.Context) codec.Inbound {
	id, err := randomID()
	return &remoteCall{remoteInbound: remote, ctx: ctx, responseID: id, err: err}
}

type remoteCall struct {
	*remoteInbound
	ctx        context.Context
	responseID string
	lease      *Lease
	err        error
}

func (call *remoteCall) ready(ctx context.Context) error {
	if call.err != nil {
		return call.err
	}
	if call.lease == nil {
		lease, err := call.manager.AcquireNative(ctx, call.module)
		if err != nil {
			return err
		}
		if lease.Revision() != call.revision {
			lease.Close()
			return errors.New("extension changed before the native request began")
		}
		call.lease = lease
		pinInboundLease(ctx, lease)
	}
	return nil
}

func (call *remoteCall) Decode(ctx context.Context, headers http.Header, body []byte) (codec.Request, error) {
	if err := call.ready(ctx); err != nil {
		if cause := ctx.Err(); cause != nil {
			return codec.Request{}, cause
		}
		return codec.Request{}, &codec.Error{Status: http.StatusBadGateway, Message: "native protocol extension is unavailable"}
	}
	// identifyCaller has replaced the request's authorization with Magpie's
	// own transport key. It is host authority, never native codec input.
	headers = headers.Clone()
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "Proxy-Authorization"} {
		headers.Del(name)
	}
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "x-magpie-") {
			headers.Del(name)
		}
	}
	if caller := CallerIDFrom(ctx); caller != "" {
		headers.Set("X-Magpie-Caller-ID", caller)
	}
	result, err := call.lease.Call(ctx, sdk.MethodInboundDecode, sdk.Scope{Plugin: call.module}, sdk.NativeRequest{
		ID: call.descriptor.ID, ResponseID: call.responseID, Headers: headers, Body: body,
	}, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return codec.Request{}, err
		}
		var public *sdk.Error
		if errors.As(err, &public) && public != nil && public.Code != "internal" {
			status := public.HTTPStatus()
			if status == 0 {
				status = http.StatusBadRequest
			}
			return codec.Request{}, &codec.Error{Status: status, Message: public.Message}
		}
		return codec.Request{}, errors.New("native extension decoding failed")
	}
	var request sdk.DecodedRequest
	if json.Unmarshal(result, &request) != nil || request.TimeoutMs < 0 ||
		request.TimeoutMs > int64((24*time.Hour)/time.Millisecond) {
		return codec.Request{}, errors.New("invalid native extension request")
	}
	return codec.Request{
		Protocol: codec.Protocol(request.Protocol), Body: request.Body,
		Session: request.Session, Timeout: time.Duration(request.TimeoutMs) * time.Millisecond,
		RequestID: request.RequestID, Metadata: bytes.Clone(request.Metadata),
	}, nil
}

func (call *remoteCall) Reply(writer http.ResponseWriter) codec.Response {
	return &remoteResponse{call: call, writer: writer, header: make(http.Header)}
}

type remoteResponse struct {
	call             *remoteCall
	writer           http.ResponseWriter
	header           http.Header
	canonicalStarted bool
	nativeStarted    bool
	finished         bool
	err              error
}

func (reply *remoteResponse) Header() http.Header { return reply.header }

// SetContext includes the deadline the codec decoded from its native request.
// It is set before any canonical response is written.
func (reply *remoteResponse) SetContext(ctx context.Context) { reply.call.ctx = ctx }

func (reply *remoteResponse) WriteHeader(status int) {
	if reply.canonicalStarted || reply.err != nil {
		return
	}
	reply.canonicalStarted = true
	reply.err = reply.send(reply.call.ctx, sdk.ResponseEvent{
		Action: sdk.ResponseStart, Status: status, Headers: reply.header.Clone(),
	})
}

func (reply *remoteResponse) Write(body []byte) (int, error) {
	if reply.finished {
		return 0, errors.New("native response is finished")
	}
	if !reply.canonicalStarted {
		reply.WriteHeader(http.StatusOK)
	}
	written := 0
	for len(body) > 0 && reply.err == nil {
		size := min(len(body), sdk.MaxChunkBytes)
		reply.err = reply.send(reply.call.ctx, sdk.ResponseEvent{Action: sdk.ResponseWrite, Body: body[:size]})
		if reply.err == nil {
			written += size
			body = body[size:]
		}
	}
	return written, reply.err
}

func (reply *remoteResponse) Flush() {
	if !reply.canonicalStarted {
		reply.WriteHeader(http.StatusOK)
	}
	if reply.err == nil {
		reply.err = reply.send(reply.call.ctx, sdk.ResponseEvent{Action: sdk.ResponseFlush})
	}
}

func (reply *remoteResponse) send(ctx context.Context, event sdk.ResponseEvent) error {
	if err := reply.call.ready(ctx); err != nil {
		return err
	}
	event.ID, event.ResponseID = reply.call.descriptor.ID, reply.call.responseID
	_, err := reply.call.lease.Call(ctx, sdk.MethodInboundResponse,
		sdk.Scope{Plugin: reply.call.module}, event, reply.native)
	return err
}

func (reply *remoteResponse) native(event Event) error {
	switch event.Name {
	case "headers":
		var headers sdk.HeadersEvent
		if json.Unmarshal(event.Data, &headers) != nil || headers.Status < 200 || headers.Status > 599 ||
			reply.nativeStarted || !validHeaders(headers.Headers) {
			return errors.New("invalid native extension response headers")
		}
		for name, values := range headers.Headers {
			if !hopHeader(name) {
				reply.writer.Header()[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
			}
		}
		reply.nativeStarted = true
		reply.writer.WriteHeader(headers.Status)
	case "data":
		var body []byte
		if json.Unmarshal(event.Data, &body) != nil || len(body) > sdk.MaxChunkBytes {
			return errors.New("invalid native extension response chunk")
		}
		if !reply.nativeStarted {
			reply.nativeStarted = true
			reply.writer.WriteHeader(http.StatusOK)
		}
		n, err := reply.writer.Write(body)
		if err != nil {
			return err
		}
		if n != len(body) {
			return errors.New("native extension client stopped reading")
		}
	case "flush":
		if flusher, ok := reply.writer.(http.Flusher); ok {
			flusher.Flush()
		}
	default:
		return errors.New("native extension emitted an unsupported event")
	}
	return nil
}

func (reply *remoteResponse) Finish(cause error) error {
	if reply.finished {
		return errors.New("native response finished more than once")
	}
	reply.finished = true
	defer func() {
		if reply.call.lease != nil {
			reply.call.lease.Close()
		}
	}()
	if reply.call.lease == nil && reply.err == nil {
		if err := reply.call.ready(reply.call.ctx); err != nil {
			reply.err = err
		}
	}
	if reply.call.lease != nil {
		if reply.err == nil && cause == nil {
			reply.err = reply.send(reply.call.ctx, sdk.ResponseEvent{Action: sdk.ResponseFinish})
		}
		if reply.err != nil || cause != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			code := "canceled"
			if errors.Is(cause, context.DeadlineExceeded) || errors.Is(reply.err, context.DeadlineExceeded) {
				code = "deadline"
			}
			// Cancellation is cleanup for this already acquired response ID.
			// It cannot select another endpoint, scope or host operation.
			// The codec may still emit its native terminal cancellation frame.
			_, _ = reply.call.lease.client.call(ctx, sdk.MethodInboundResponse,
				sdk.Scope{Plugin: reply.call.module}, nil, sdk.ResponseEvent{
					ID: reply.call.descriptor.ID, ResponseID: reply.call.responseID,
					Action: sdk.ResponseCancel, Error: &sdk.Error{Code: code, Message: "native request ended"},
				}, reply.native)
		}
	}
	if reply.err != nil && !reply.nativeStarted {
		reply.writer.Header().Set("Content-Type", "application/json")
		reply.writer.WriteHeader(http.StatusBadGateway)
		_, _ = reply.writer.Write([]byte(`{"error":{"message":"native protocol extension failed"}}`))
	}
	return reply.err
}

func validHeaders(headers http.Header) bool {
	total := 0
	for name, values := range headers {
		if name == "" {
			return false
		}
		for _, char := range name {
			if char > 127 || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
				char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char)) {
				return false
			}
		}
		for _, value := range values {
			total += len(name) + len(value)
			if total > 64<<10 || strings.ContainsAny(value, "\r\n\x00") {
				return false
			}
		}
	}
	return true
}

func hopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}
