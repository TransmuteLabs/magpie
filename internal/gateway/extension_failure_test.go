package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestUpstreamFailureKeepsOnlyBoundedExplicitMachineFields(t *testing.T) {
	for _, sample := range []struct{ name, body, code, typ string }{
		{"nested", `{"error":{"code":"credits_empty","type":"quota_error","message":"PRIVATE_TOKEN"}}`, "credits_empty", "quota_error"},
		{"numeric", `{"error":{"code":1008,"status":"RESOURCE_EXHAUSTED"}}`, "1008", "RESOURCE_EXHAUSTED"},
		{"root", `{"code":"denied","type":"permission_error"}`, "denied", "permission_error"},
		{"missing", `{}`, "", ""},
		{"null", `{"error":null,"code":"not_error_metadata"}`, "", ""},
		{"nested excludes metadata", `{"code":"not_error_metadata","type":"object","error":{}}`, "", ""},
		{"malformed", `{"error":`, "", ""},
		{"trailing", `{"error":{"code":"denied"}} {}`, "", ""},
		{"wrong types", `{"error":{"code":true,"type":[]}}`, "", ""},
		{"private text", `{"error":{"code":"Bearer PRIVATE_TOKEN","type":"one\ntwo","message":"PRIVATE_TOKEN"}}`, "", ""},
		{"long code", `{"error":{"code":"` + strings.Repeat("a", 257) + `","type":"quota"}}`, "", "quota"},
		{"long type", `{"error":{"code":"denied","type":"` + strings.Repeat("a", 129) + `"}}`, "denied", ""},
		{"large body", `{"error":{"code":"denied","type":"quota","message":"` + strings.Repeat("a", 64<<10) + `"}}`, "", ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			got := upstreamFailure(429, []byte(sample.body))
			if got.Status != 429 || got.Code != sample.code || got.Type != sample.typ {
				t.Fatalf("unexpected snapshot: %+v", got)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "PRIVATE_") {
				t.Fatal("snapshot retained private text")
			}
		})
	}
}

func TestUpstreamFailureCapturePrecedesTranslationAndDoesNotChangeReply(t *testing.T) {
	held := &holdWriter{header: make(http.Header), hold: true}
	response := &http.Response{StatusCode: 413, Header: http.Header{"Content-Type": {"application/json"}}}
	body := []byte(`{"error":{"code":"vendor_size","type":"vendor_request","message":"prompt is too long"}}`)
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	request := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(withFailureSink(context.Background(), "fixture", &held.upstreamFailure))
	upstreamBegins(request, "fixture")(response, nil)
	gotBody, err := io.ReadAll(response.Body)
	if err != nil || string(gotBody) != string(body) {
		t.Fatal("observing changed response bytes", err)
	}
	response.Body.Close()
	status := writeVendorErrorFrom(held, provider.Chat, provider.Chat, provider.Provider{}, response, "prompt is too long", body)
	if status != 400 || held.upstreamFailure.snapshot() != (usage.UpstreamFailure{Status: 413, Code: "vendor_size", Type: "vendor_request"}) {
		t.Fatalf("translation replaced observation: status=%d snapshot=%+v", status, held.upstreamFailure.snapshot())
	}
	copy := held.upstreamFailure.snapshot()
	copy.Code = "changed"
	if held.upstreamFailure.snapshot().Code != "vendor_size" {
		t.Fatal("snapshot shared mutable state")
	}
	for _, value := range []any{Call{}, Route{}, Try{}, usage.Record{}} {
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), "upstream_failure") {
			t.Fatal("absent failure became synthetic facts")
		}
	}
}

func TestUpstreamFailureObserverSelectionAndStaleCompletion(t *testing.T) {
	var observation failureObservation
	request := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(withFailureSink(context.Background(), "selected", &observation))
	for _, scenario := range []struct {
		name, id string
		status   int
		sink     bool
	}{
		{"success", "selected", 200, true}, {"upgrade", "selected", 101, true},
		{"other provider", "other", 403, true}, {"non-plugin", "", 403, true}, {"no sink", "selected", 403, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			body := &failureTestBody{reader: strings.NewReader("unread")}
			response := &http.Response{StatusCode: scenario.status, Body: body}
			req := request
			if !scenario.sink {
				req = request.WithContext(context.Background())
			}
			upstreamBegins(req, scenario.id)(response, nil)
			if response.Body != body || body.reads != 0 || body.closes != 0 {
				t.Fatal("unrelated/success body was touched")
			}
		})
	}
	first := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"code":"old"}`))}
	second := &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"code":"new"}`))}
	upstreamBegins(request, "selected")(first, nil)
	if observation.snapshot().Status != 403 {
		t.Fatal("status waits for body")
	}
	upstreamBegins(request, "selected")(second, nil)
	io.Copy(io.Discard, second.Body)
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	second.Body.Close()
	upstreamBegins(request, "selected")(nil, errors.New("local fence"))
	if observation.snapshot() != (usage.UpstreamFailure{Status: 429, Code: "new"}) {
		t.Fatalf("stale response or local fence replaced snapshot: %+v", observation.snapshot())
	}
	late := &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"code":"stale"}`))}
	upstreamBegins(request, "selected")(late, nil)
	upstreamBegins(request, "selected")(&http.Response{StatusCode: 200}, nil)
	io.Copy(io.Discard, late.Body)
	late.Body.Close()
	if observation.snapshot() != (usage.UpstreamFailure{}) {
		t.Fatal("a successful response retained an earlier refusal")
	}
}

type failureTestBody struct {
	reader        io.Reader
	reads, closes int
	closeError    error
}

func (b *failureTestBody) Read(p []byte) (int, error) { b.reads++; return b.reader.Read(p) }
func (b *failureTestBody) Close() error               { b.closes++; return b.closeError }

type failureReadError struct{ err error }

func (r failureReadError) Read([]byte) (int, error) { return 0, r.err }

func TestUpstreamFailureObserverBoundsAndReadErrors(t *testing.T) {
	readError, closeError := errors.New("read failed"), errors.New("close failed")
	valid := `{"code":"bounded","type":"refusal"}`
	for _, sample := range []struct {
		name      string
		reader    io.Reader
		wantCode  string
		wantError error
	}{
		{"complete", strings.NewReader(valid), "bounded", nil},
		{"exact limit", strings.NewReader(valid + strings.Repeat(" ", failureBodyLimit-len(valid))), "bounded", nil},
		{"over limit", strings.NewReader(valid + strings.Repeat(" ", failureBodyLimit-len(valid)+1)), "", nil},
		{"malformed", strings.NewReader(valid + "{"), "", nil},
		{"read error", io.MultiReader(strings.NewReader(valid), failureReadError{readError}), "", readError},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var observation failureObservation
			ctx := withFailureSink(context.Background(), "fixture", &observation)
			body := &failureTestBody{reader: sample.reader, closeError: closeError}
			response := &http.Response{StatusCode: 403, Body: body}
			observeUpstreamFailure(ctx, "fixture", response)
			if body.reads != 0 {
				t.Fatal("eager body read")
			}
			_, err := io.ReadAll(response.Body)
			if err != sample.wantError || response.Body.Close() != closeError || body.closes != 1 {
				t.Fatal("read/close error changed", err)
			}
			if got := observation.snapshot(); got.Status != 403 || got.Code != sample.wantCode {
				t.Fatalf("snapshot=%+v", got)
			}
			b := response.Body.(*failureBody)
			if len(b.data) != 0 {
				t.Fatal("raw body retained after completion")
			}
		})
	}
}

func TestUpstreamFailureObserverCloseInterruptsRead(t *testing.T) {
	var observation failureObservation
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	response := &http.Response{StatusCode: 429, Body: reader}
	observeUpstreamFailure(withFailureSink(context.Background(), "fixture", &observation), "fixture", response)
	read := make(chan error, 1)
	go func() { _, err := io.ReadAll(response.Body); read <- err }()
	closed := make(chan error, 1)
	go func() { closed <- response.Body.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on Read")
	}
	select {
	case err := <-read:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("read error changed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not unblock")
	}
	if observation.snapshot() != (usage.UpstreamFailure{Status: 429}) {
		t.Fatal("incomplete body supplied fields")
	}
}

func TestUpstreamFailureObserverIncompleteCloseRetainsOnlyStatus(t *testing.T) {
	var observation failureObservation
	response := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"code":"not_complete"}`))}
	observeUpstreamFailure(withFailureSink(context.Background(), "fixture", &observation), "fixture", response)
	buffer := make([]byte, 100)
	n, err := response.Body.Read(buffer)
	if n == 0 || err != nil {
		t.Fatal("fixture did not stop before EOF", n, err)
	}
	response.Body.Close()
	if observation.snapshot() != (usage.UpstreamFailure{Status: 403}) {
		t.Fatal("a prefix without EOF supplied fields")
	}
}

type failureSDKTestBody struct {
	*failureTestBody
	complete   bool
	closedWith error
}

func (b *failureSDKTestBody) ExtensionUsage() *sdk.Usage     { return &sdk.Usage{InputTokens: 7} }
func (b *failureSDKTestBody) ExtensionProtocolComplete()     { b.complete = true }
func (b *failureSDKTestBody) CloseWithError(err error) error { b.closedWith = err; return err }

type failureWriterTestBody struct {
	*failureTestBody
	writes, transfers int
}

func (b *failureWriterTestBody) Write(p []byte) (int, error) { b.writes++; return len(p), nil }
func (b *failureWriterTestBody) WriteTo(w io.Writer) (int64, error) {
	b.transfers++
	return io.Copy(w, b.reader)
}

func TestUpstreamFailureObserverPreservesBodyInterfaces(t *testing.T) {
	var observation failureObservation
	ctx := withFailureSink(context.Background(), "fixture", &observation)
	sdkBody := &failureSDKTestBody{failureTestBody: &failureTestBody{reader: strings.NewReader(`{"code":"sdk"}`)}}
	response := &http.Response{StatusCode: 403, Body: sdkBody}
	observeUpstreamFailure(ctx, "fixture", response)
	capabilities, ok := response.Body.(failureSDKResponse)
	if !ok || capabilities.ExtensionUsage().InputTokens != 7 {
		t.Fatal("SDK usage lost")
	}
	capabilities.ExtensionProtocolComplete()
	marker := errors.New("cancel")
	if capabilities.CloseWithError(marker) != marker || !sdkBody.complete || sdkBody.closedWith != marker {
		t.Fatal("SDK completion/cancellation lost")
	}
	response.Body.Close()
	writer := &failureWriterTestBody{failureTestBody: &failureTestBody{reader: strings.NewReader(`{"code":"copy"}`)}}
	response = &http.Response{StatusCode: 429, Body: writer}
	observeUpstreamFailure(ctx, "fixture", response)
	response.Body.(io.Writer).Write([]byte("sent"))
	var copied strings.Builder
	n, err := io.Copy(&copied, response.Body)
	if err != nil || n != int64(len(copied.String())) || copied.String() != `{"code":"copy"}` || writer.writes != 1 || writer.transfers != 1 {
		t.Fatal("optional I/O interfaces changed", n, err)
	}
	if observation.snapshot().Code != "copy" {
		t.Fatal("WriterTo bypassed observation")
	}
	response.Body.Close()
}

func TestUpstreamFailureDiagnosticDedupKeepsDistinctFacts(t *testing.T) {
	call := Call{Agent: t.Name(), Provider: "fixture", Model: "m1", Status: 409, Error: "operation terminal", UpstreamFailure: usage.UpstreamFailure{Status: 429, Code: "rate", Type: "vendor_refusal"}}
	now := time.Now()
	if _, ok := failedCallLine(call, now); !ok {
		t.Fatal("first failure missing")
	}
	if _, ok := failedCallLine(call, now); ok {
		t.Fatal("identical failure was not deduplicated")
	}
	call.UpstreamFailure.Code = "credits"
	if line, ok := failedCallLine(call, now); !ok || !strings.Contains(line, "upstream_code=credits") {
		t.Fatal("distinct refusal was hidden by dedup")
	}
}

type failurePrefixDestination struct{ bytes int }

func (w failurePrefixDestination) Write(p []byte) (int, error) {
	return min(w.bytes, len(p)), io.EOF
}

func TestUpstreamFailureWriterDestinationEOFIsNotSourceEOF(t *testing.T) {
	const prefix = `{"code":"incomplete"}`
	var observation failureObservation
	response := &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(prefix + strings.Repeat(" ", 100)))}
	observeUpstreamFailure(withFailureSink(context.Background(), "fixture", &observation), "fixture", response)
	n, err := io.Copy(failurePrefixDestination{len(prefix)}, response.Body)
	if n != int64(len(prefix)) || err != io.EOF {
		t.Fatal("copy result changed", n, err)
	}
	response.Body.Close()
	if observation.snapshot() != (usage.UpstreamFailure{Status: 429}) {
		t.Fatalf("destination error promoted incomplete prefix: %+v", observation.snapshot())
	}
}
