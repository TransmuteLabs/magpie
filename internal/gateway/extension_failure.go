package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const failureBodyLimit = 64 << 10

type failureSinkKey struct{}
type failureSink struct {
	id          string
	observation *failureObservation
}

// Only the terminal extension branch publishes this observation. It grants
// no retry authority and never supplies a refusal to routing policy.
type failureObservation struct {
	mu       sync.Mutex
	sequence uint64
	failure  usage.UpstreamFailure
}

func withFailureSink(ctx context.Context, id string, observation *failureObservation) context.Context {
	return context.WithValue(ctx, failureSinkKey{}, failureSink{id, observation})
}

func (o *failureObservation) snapshot() usage.UpstreamFailure {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failure
}

// Observe at the response boundary, before format repair or error translation.
// A later local transport error has no response and cannot erase the refusal.
func observeUpstreamFailure(ctx context.Context, id string, res *http.Response) {
	sink, ok := ctx.Value(failureSinkKey{}).(failureSink)
	if !ok || id == "" || sink.id != id || res == nil {
		return
	}
	o := sink.observation
	o.mu.Lock()
	o.sequence++
	sequence := o.sequence
	o.failure = usage.UpstreamFailure{}
	if observation, generated := provider.ExtensionRefusalObservation(res); generated {
		o.failure = usage.UpstreamFailure(observation)
		o.mu.Unlock()
		return
	}
	refused := res.StatusCode >= 400 && res.StatusCode <= 599
	if refused {
		o.failure.Status = res.StatusCode
	}
	o.mu.Unlock()
	if !refused || res.Body == nil {
		return
	}
	b := &failureBody{ReadCloser: res.Body, status: res.StatusCode, put: func(failure usage.UpstreamFailure) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.sequence == sequence {
			o.failure = failure
		}
	}}
	// Preserve the SDK body's usage, protocol completion and cancellation
	// methods. Successes (including streams and upgrades) are never wrapped.
	if sdkBody, ok := res.Body.(failureSDKResponse); ok {
		res.Body = &failureSDKBody{failureBody: b, failureSDKResponse: sdkBody}
		return
	}
	var observed io.ReadCloser = b
	if wt, ok := res.Body.(io.WriterTo); ok {
		observed = &failureWriterTo{failureBody: b, source: wt}
	}
	if writer, ok := res.Body.(io.Writer); ok {
		if wt, ok := observed.(io.WriterTo); ok {
			res.Body = struct {
				io.ReadCloser
				io.Writer
				io.WriterTo
			}{observed, writer, wt}
		} else {
			res.Body = struct {
				io.ReadCloser
				io.Writer
			}{observed, writer}
		}
		return
	}
	res.Body = observed
}

// Collect only bytes the ordinary consumer reads. Close can interrupt a
// blocked Read; neither calls the source with the observation mutex held.
type failureBody struct {
	io.ReadCloser
	mu     sync.Mutex
	status int
	data   []byte
	done   bool
	put    func(usage.UpstreamFailure)
}

func (b *failureBody) note(data []byte, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	if len(data) > failureBodyLimit-len(b.data) {
		b.data, b.done = nil, true
		return
	}
	b.data = append(b.data, data...)
	if err == io.EOF {
		b.put(upstreamFailure(b.status, b.data))
	}
	if err != nil {
		b.data, b.done = nil, true
	}
}

func (b *failureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.note(p[:n], err)
	return n, err
}

func (b *failureBody) discard() {
	b.mu.Lock()
	b.data, b.done = nil, true
	b.mu.Unlock()
}

func (b *failureBody) Close() error {
	b.discard()
	return b.ReadCloser.Close()
}

type failureSDKResponse interface {
	ExtensionUsage() *sdk.Usage
	ExtensionProtocolComplete()
	CloseWithError(error) error
}

type failureSDKBody struct {
	*failureBody
	failureSDKResponse
}

func (b *failureSDKBody) CloseWithError(err error) error {
	b.discard()
	return b.failureSDKResponse.CloseWithError(err)
}

type failureWriterTo struct {
	*failureBody
	source io.WriterTo
}

func (b *failureWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, err := b.source.WriteTo(failureWriteTap{w, b.failureBody})
	if err == nil {
		b.note(nil, io.EOF)
	} else {
		b.discard()
	}
	return n, err
}

type failureWriteTap struct {
	io.Writer
	body *failureBody
}

func (w failureWriteTap) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		// A destination's EOF does not complete the source body.
		w.body.discard()
	} else {
		w.body.note(p[:n], nil)
	}
	return n, err
}

func upstreamFailure(status int, body []byte) usage.UpstreamFailure {
	result := usage.UpstreamFailure{Status: status}
	if len(body) > failureBodyLimit {
		return result
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return result
	}
	if nested, ok := envelope["error"]; ok {
		var fields map[string]json.RawMessage
		if json.Unmarshal(nested, &fields) != nil {
			return result
		}
		envelope = fields
	}
	var code, typ string
	if raw := envelope["code"]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &code) != nil {
			var number json.Number
			if json.Unmarshal(raw, &number) == nil {
				code = number.String()
			}
		}
	}
	_ = json.Unmarshal(envelope["type"], &typ)
	if typ == "" {
		_ = json.Unmarshal(envelope["status"], &typ)
	}
	return usage.UpstreamFailure(requestpolicy.ObserveResponse(status, code, typ))
}

func upstreamFailureLine(failure usage.UpstreamFailure) string {
	if failure.Status == 0 {
		return ""
	}
	line := " upstream_status=" + strconv.Itoa(failure.Status)
	if failure.Code != "" {
		line += " upstream_code=" + failure.Code
	}
	if failure.Type != "" {
		line += " upstream_type=" + failure.Type
	}
	return line
}
