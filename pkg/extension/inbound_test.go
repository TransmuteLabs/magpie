package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/pkg/protocolext"
)

type fixtureCodec struct {
	binds       atomic.Int64
	finishes    atomic.Int64
	mu          sync.Mutex
	contexts    []context.Context
	metadata    json.RawMessage
	protocol    protocolext.Protocol
	contract    protocolext.Contract
	decodedBody []byte
}

func (codec *fixtureCodec) Contract() protocolext.Contract {
	if codec.contract.Capabilities != nil {
		return codec.contract
	}
	return protocolext.CatalogContract()
}
func (*fixtureCodec) ID() string   { return "codec" }
func (*fixtureCodec) Path() string { return "/fixture.Service/Call" }
func (codec *fixtureCodec) Bind(ctx context.Context) protocolext.Inbound {
	instance := codec.binds.Add(1)
	codec.mu.Lock()
	codec.contexts = append(codec.contexts, ctx)
	codec.mu.Unlock()
	return &boundFixtureCodec{source: codec, instance: instance}
}
func (*fixtureCodec) Decode(context.Context, http.Header, []byte) (protocolext.Request, error) {
	return protocolext.Request{}, fmt.Errorf("unbound Decode")
}
func (*fixtureCodec) Reply(http.ResponseWriter) protocolext.Response { return nil }

type boundFixtureCodec struct {
	source   *fixtureCodec
	instance int64
	replied  bool
}

func (*boundFixtureCodec) Contract() protocolext.Contract {
	return protocolext.Contract{Version: protocolext.APIVersion{Major: 99}}
}
func (*boundFixtureCodec) ID() string   { return "cannot-change-authority" }
func (*boundFixtureCodec) Path() string { return "/cannot-change-authority" }
func (codec *boundFixtureCodec) Decode(_ context.Context, _ http.Header, body []byte) (protocolext.Request, error) {
	if !codec.replied {
		return protocolext.Request{}, fmt.Errorf("Reply was not called before Decode")
	}
	if string(body) == "models" {
		return protocolext.Request{Protocol: protocolext.Models}, nil
	}
	metadata := codec.source.metadata
	if metadata == nil {
		metadata = json.RawMessage(`{"option":"native"}`)
	}
	protocol := codec.source.protocol
	if protocol == "" {
		protocol = protocolext.Chat
	}
	canonical := codec.source.decodedBody
	if canonical == nil {
		canonical = []byte(`{"model":"fixture/model","messages":[]}`)
	}
	return protocolext.Request{
		Protocol: protocol, Body: canonical,
		Session: fmt.Sprint(codec.instance), Timeout: 1500 * time.Microsecond,
		RequestID: "native-operation", Metadata: metadata,
	}, nil
}
func (codec *boundFixtureCodec) Reply(writer http.ResponseWriter) protocolext.Response {
	codec.replied = true
	return &fixtureCodecResponse{ResponseWriter: writer, codec: codec}
}

type fixtureCodecResponse struct {
	http.ResponseWriter
	codec *boundFixtureCodec
}

func (response *fixtureCodecResponse) WriteHeader(status int) {
	response.ResponseWriter.Header().Set("Content-Type", "application/x-fixture")
	response.ResponseWriter.Header().Set("X-Fixture-Instance", fmt.Sprint(response.codec.instance))
	response.ResponseWriter.WriteHeader(status)
}

func (response *fixtureCodecResponse) Write(body []byte) (int, error) {
	_, err := fmt.Fprintf(response.ResponseWriter, "%d:%s", response.codec.instance, body)
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

func (response *fixtureCodecResponse) Flush() {
	response.ResponseWriter.(http.Flusher).Flush()
}

func (response *fixtureCodecResponse) Finish(error) error {
	response.codec.source.finishes.Add(1)
	return nil
}

func inboundFixtureModule(codec *fixtureCodec) Module {
	manifest := testManifest(InboundCodec, ModelDiscovery)
	manifest.Inbounds = []InboundDescriptor{{ID: codec.ID(), Path: codec.Path()}}
	return Module{Manifest: manifest, Inbounds: []protocolext.Inbound{codec}}
}

func completeRPC(t *testing.T, peer *testPeer, frame Frame) (Frame, []Frame) {
	t.Helper()
	peer.send(frame)
	var events []Frame
	for {
		reply := peer.read()
		if reply.ID != frame.ID {
			t.Fatalf("another response escaped its call: %+v", reply)
		}
		if reply.Kind == KindResult {
			return reply, events
		}
		if reply.Kind != KindEvent {
			t.Fatalf("unexpected codec frame: %+v", reply)
		}
		events = append(events, reply)
		peer.send(Frame{Kind: KindAck, ID: reply.ID, Seq: reply.Seq})
	}
}

func TestInboundHelpersBindOnceBeforeDecodeAndIsolateResponses(t *testing.T) {
	codec := &fixtureCodec{}
	module := inboundFixtureModule(codec)
	if err := module.Validate(); err != nil {
		t.Fatal("helper did not install both required inbound methods", err)
	}
	peer := startPeer(t, module)
	for index, responseID := range []string{"response-a", "response-b"} {
		call := callFrame("decode-"+responseID, MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: responseID, Body: []byte("native")})
		result, events := completeRPC(t, peer, call)
		var decoded DecodedRequest
		if result.Error != nil || len(events) != 0 || json.Unmarshal(result.Result, &decoded) != nil ||
			decoded.Session != fmt.Sprint(index+1) || decoded.Protocol != "chat" || decoded.TimeoutMs != 2 ||
			decoded.RequestID != "native-operation" || string(decoded.Metadata) != `{"option":"native"}` {
			t.Fatalf("codec did not reuse its bound Reply/Decode instance: %+v events=%+v decoded=%+v", result, events, decoded)
		}
	}
	if codec.binds.Load() != 2 {
		t.Fatalf("bound %d instances for two responses", codec.binds.Load())
	}
	codec.mu.Lock()
	contexts := append([]context.Context(nil), codec.contexts...)
	codec.mu.Unlock()
	for _, ctx := range contexts {
		if ctx.Err() != nil {
			t.Fatal("successful decode canceled the response lifetime")
		}
	}
	for index, responseID := range []string{"response-a", "response-b"} {
		start := ResponseEvent{ID: "codec", ResponseID: responseID, Action: ResponseStart, Status: 201, Headers: http.Header{"X-Canonical": {"value"}}}
		result, events := completeRPC(t, peer, callFrame("start-"+responseID, MethodInboundResponse, start))
		var header HeadersEvent
		if result.Error != nil || len(events) != 1 || events[0].Event != "headers" || json.Unmarshal(events[0].Data, &header) != nil ||
			header.Status != 201 || header.Headers.Get("X-Fixture-Instance") != fmt.Sprint(index+1) {
			t.Fatalf("native headers: result=%+v events=%+v", result, events)
		}
		write := ResponseEvent{ID: "codec", ResponseID: responseID, Action: ResponseWrite, Body: []byte("canonical")}
		result, events = completeRPC(t, peer, callFrame("write-"+responseID, MethodInboundResponse, write))
		var body []byte
		if result.Error != nil || len(events) != 1 || json.Unmarshal(events[0].Data, &body) != nil || string(body) != fmt.Sprintf("%d:canonical", index+1) {
			t.Fatalf("codec states mixed: %+v events=%+v body=%q", result, events, body)
		}
		flush := ResponseEvent{ID: "codec", ResponseID: responseID, Action: ResponseFlush}
		result, events = completeRPC(t, peer, callFrame("flush-"+responseID, MethodInboundResponse, flush))
		if result.Error != nil || len(events) != 1 || events[0].Event != "flush" {
			t.Fatal("flush was not acknowledged", result, events)
		}
		finish := ResponseEvent{ID: "codec", ResponseID: responseID, Action: ResponseFinish}
		result, events = completeRPC(t, peer, callFrame("finish-"+responseID, MethodInboundResponse, finish))
		if result.Error != nil || len(events) != 0 || contexts[index].Err() != context.Canceled {
			t.Fatal("finish did not release response lifetime", result, events)
		}
	}
	if codec.finishes.Load() != 2 {
		t.Fatal("Finish was not called exactly once per response", codec.finishes.Load())
	}
}

func TestInboundHelperRefusesAnotherScopeAndUnnegotiatedDiscovery(t *testing.T) {
	codec := &fixtureCodec{}
	module := inboundFixtureModule(codec)
	module.Manifest.Contract.Capabilities = []Capability{InboundCodec}
	peer := startPeer(t, module)
	call := callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "response", Body: []byte("models")})
	call.Scope.Account, call.Scope.Generation = "first", 1
	result, _ := completeRPC(t, peer, call)
	if result.Error == nil || result.Error.Code != "permission_denied" {
		t.Fatal("codec acquired unadvertised discovery", result)
	}
	write := callFrame("other-scope", MethodInboundResponse, ResponseEvent{ID: "codec", ResponseID: "response", Action: ResponseWrite, Body: []byte("data")})
	write.Scope.Account, write.Scope.Generation = "second", 2
	result, _ = completeRPC(t, peer, write)
	if result.Error == nil || result.Error.Code != "permission_denied" {
		t.Fatal("response escaped its account generation", result)
	}
	cancel := callFrame("cancel", MethodInboundResponse, ResponseEvent{ID: "codec", ResponseID: "response", Action: ResponseCancel})
	cancel.Scope = call.Scope
	result, _ = completeRPC(t, peer, cancel)
	if result.Error != nil || codec.finishes.Load() != 1 {
		t.Fatal("cancel did not finish response", result)
	}
	result, _ = completeRPC(t, peer, callFrame("unknown", MethodInboundResponse, ResponseEvent{ID: "codec", ResponseID: "response", Action: ResponseWrite}))
	if result.Error == nil || result.Error.Code != "invalid_params" {
		t.Fatal("closed response was resurrected", result)
	}
}

func TestInboundHelperChecksDescriptorsAndCleansUpOnEOF(t *testing.T) {
	codec := &fixtureCodec{}
	module := inboundFixtureModule(codec)
	bad := module
	bad.Manifest = cloneManifest(module.Manifest)
	bad.Manifest.Inbounds[0].ID = "mismatch"
	if err := bad.Validate(); err == nil {
		t.Fatal("codec descriptor mismatch accepted")
	}
	peer := startPeer(t, module)
	result, _ := completeRPC(t, peer, callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "response", Body: []byte("models")}))
	if result.Error != nil {
		t.Fatal("negotiated inbound discovery failed", result.Error)
	}
	peer.input.Close()
	if err := peer.wait(); err != nil || codec.finishes.Load() != 1 {
		t.Fatal("server did not release an unfinished response", err, codec.finishes.Load())
	}
}

func TestInboundHelperPreservesCanonicalProtocolsAndBoundsMetadata(t *testing.T) {
	for _, protocol := range []protocolext.Protocol{protocolext.Chat, protocolext.Responses, protocolext.Messages, protocolext.Anthropic, protocolext.Gemini} {
		t.Run(string(protocol), func(t *testing.T) {
			codec := &fixtureCodec{protocol: protocol}
			peer := startPeer(t, inboundFixtureModule(codec))
			result, _ := completeRPC(t, peer, callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "response", Body: []byte("native")}))
			var decoded DecodedRequest
			if result.Error != nil || json.Unmarshal(result.Result, &decoded) != nil || decoded.Protocol != string(protocol) {
				t.Fatal("canonical protocol was changed", result, decoded)
			}
		})
	}
	for _, test := range []struct {
		name     string
		metadata json.RawMessage
		valid    bool
	}{
		{name: "exact", metadata: json.RawMessage(`"` + strings.Repeat("x", MaxChunkBytes-2) + `"`), valid: true},
		{name: "oversized", metadata: json.RawMessage(`"` + strings.Repeat("x", MaxChunkBytes-1) + `"`)},
		{name: "malformed", metadata: json.RawMessage(`{`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			codec := &fixtureCodec{metadata: test.metadata}
			peer := startPeer(t, inboundFixtureModule(codec))
			result, _ := completeRPC(t, peer, callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "response", Body: []byte("native")}))
			if (result.Error == nil) != test.valid {
				t.Fatalf("metadata bound: valid=%v error=%v", test.valid, result.Error)
			}
		})
	}
}

func TestInboundHelperSplitsNativeOutputIntoAcknowledgedChunks(t *testing.T) {
	codec := &fixtureCodec{}
	peer := startPeer(t, inboundFixtureModule(codec))
	result, _ := completeRPC(t, peer, callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "response", Body: []byte("native")}))
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	payload := []byte(strings.Repeat("x", MaxChunkBytes))
	result, events := completeRPC(t, peer, callFrame("write", MethodInboundResponse, ResponseEvent{ID: "codec", ResponseID: "response", Action: ResponseWrite, Body: payload}))
	var output []byte
	for _, event := range events {
		if event.Event == "data" {
			var chunk []byte
			if json.Unmarshal(event.Data, &chunk) != nil || len(chunk) > MaxChunkBytes {
				t.Fatal("native codec emitted an invalid chunk", event)
			}
			output = append(output, chunk...)
		}
	}
	if result.Error != nil || string(output) != "1:"+string(payload) || len(events) != 3 {
		t.Fatalf("native output was not split faithfully: error=%v bytes=%d events=%d", result.Error, len(output), len(events))
	}
}
