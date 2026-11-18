package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/protocolext"
)

type dictionaryBoundCounter struct {
	source io.Reader
	read   int64
	closed bool
}

func (reader *dictionaryBoundCounter) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	reader.read += int64(count)
	return count, err
}

func (reader *dictionaryBoundCounter) Close() error {
	reader.closed = true
	return nil
}

type dictionaryBoundFill struct{}

func (dictionaryBoundFill) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = 'x'
	}
	return len(buffer), nil
}

func dictionaryBoundInput(t *testing.T, size int64, encoding string, dictionary []byte) (*dictionaryBoundCounter, int64, bodySum) {
	t.Helper()
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(dictionaryBoundFill{}, size)); err != nil {
		t.Fatal(err)
	}
	var sum bodySum
	copy(sum[:], digest.Sum(nil))
	if encoding == "identity" || encoding == "unknown" {
		length := size
		if encoding == "unknown" {
			length = -1
		}
		return &dictionaryBoundCounter{source: io.LimitReader(dictionaryBoundFill{}, size)}, length, sum
	}
	var wire bytes.Buffer
	options := []zstd.EOption{zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true)}
	if encoding == "dcz" {
		dictionarySum := sha256.Sum256(dictionary)
		wire.Write(dczMagic)
		wire.Write(dictionarySum[:])
		options = append(options, zstd.WithEncoderDictRaw(0, dictionary), zstd.WithWindowSize(dictWindow(len(dictionary))))
	}
	writer, err := zstd.NewWriter(&wire, options...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(writer, io.LimitReader(dictionaryBoundFill{}, size)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &dictionaryBoundCounter{source: bytes.NewReader(wire.Bytes())}, int64(wire.Len()), sum
}

type dictionaryBoundFixture struct {
	server         *Server
	handler        http.Handler
	secret, scope  string
	decodes, sends atomic.Int64
}

func newDictionaryBoundFixture(t *testing.T, limit int64) *dictionaryBoundFixture {
	t.Helper()
	fresh(t)
	freshDicts(t)
	fixture := &dictionaryBoundFixture{}
	keys, secrets := newCaller(t, "Dictionary bound")
	fixture.secret = secrets[0]
	fixture.scope = keys[0].ID + "\x00127.0.0.1"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.sends.Add(1)
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			t.Error(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/responses") {
			io.WriteString(writer, `{"id":"resp_fixture","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	if err := provider.Save(provider.Provider{
		ID: "fixture", Name: "Fixture", Key: "fixture-key", Models: []string{"m1"},
		Chat: upstream.URL + "/v1", Responses: upstream.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}
	fixture.server = protocolServer(t, fixtureInbound{decode: func(ctx context.Context, headers http.Header, body []byte) (sdk.Request, error) {
		fixture.decodes.Add(1)
		if access.Caller(ctx).KeyID != keys[0].ID {
			t.Error("native Decode lost authenticated caller scope")
		}
		for _, name := range []string{KeepBodyHeader, availableDictHeader, BodyDigestHeader, "Content-Encoding"} {
			if headers.Get(name) != "" {
				t.Errorf("dictionary header reached Decode: %s", name)
			}
		}
		return sdk.Request{Protocol: sdk.Chat, Body: []byte(`{"model":"fixture/m1","messages":[{"role":"user","content":"hello"}]}`)}, nil
	}})
	fixture.server.requestLimits.body = limit
	fixture.handler = fixture.server.Front()
	return fixture
}

func (fixture *dictionaryBoundFixture) request(endpoint string, body io.ReadCloser, length int64) *http.Request {
	request := httptest.NewRequest(http.MethodPost, endpoint, nil)
	request.Body, request.ContentLength = body, length
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer "+fixture.secret)
	request.Header.Set("User-Agent", "magpie/0.1.9999")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(KeepBodyHeader, "dcz")
	return request
}

func (fixture *dictionaryBoundFixture) seed(t *testing.T) ([]byte, bodySum) {
	t.Helper()
	dictionary := bytes.Repeat([]byte("d"), dictMin+101)
	sum := sha256.Sum256(dictionary)
	request := fixture.request("/fixture/rpc", io.NopCloser(bytes.NewReader(dictionary)), int64(len(dictionary)))
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get(KeptBodyHeader) != sfBinary(sum) {
		t.Fatalf("same-scope dictionary seed: %d %v %s", response.Code, response.Header(), response.Body)
	}
	if heldDicts.get(fixture.scope, sum) == nil || fixture.decodes.Load() != 1 || fixture.sends.Load() != 1 {
		t.Fatal("dictionary seed did not traverse authenticated Front, Decode and provider")
	}
	return dictionary, sum
}

func TestNativeDictionaryInputBoundBeforeReadAndCache(t *testing.T) {
	for _, limit := range []int64{protocolBodyLimit, 32 << 10} {
		name := "native"
		if limit < protocolBodyLimit {
			name = "configured"
		}
		for _, encoding := range []string{"identity", "unknown", "zstd", "dcz"} {
			t.Run(name+"/"+encoding, func(t *testing.T) {
				configured := limit
				if name == "native" {
					configured = 0
				}
				fixture := newDictionaryBoundFixture(t, configured)
				var dictionary []byte
				var dictionarySum bodySum
				if encoding == "dcz" {
					dictionary, dictionarySum = fixture.seed(t)
				}
				reader, length, sum := dictionaryBoundInput(t, 2*limit, encoding, dictionary)
				if (encoding == "zstd" || encoding == "dcz") && length >= limit {
					t.Fatal("compressed fixture must fit the wire limit and exceed the decoded limit")
				}
				request := fixture.request("/fixture/rpc", reader, length)
				if encoding == "zstd" || encoding == "dcz" {
					request.Header.Set("Content-Encoding", encoding)
				}
				if encoding == "dcz" {
					request.Header.Set(availableDictHeader, sfBinary(dictionarySum))
					request.Header.Set(BodyDigestHeader, sfBinary(sum))
				}
				decodes, sends := fixture.decodes.Load(), fixture.sends.Load()
				heldDicts.mu.Lock()
				kept := heldDicts.size
				heldDicts.mu.Unlock()
				response := httptest.NewRecorder()
				fixture.handler.ServeHTTP(response, request)
				if response.Code != http.StatusRequestEntityTooLarge {
					t.Errorf("oversized %s: %d %s", encoding, response.Code, response.Body)
				}
				if encoding == "identity" && reader.read != 0 {
					t.Errorf("declared oversized body read %d bytes, want zero", reader.read)
				}
				if encoding == "unknown" && reader.read != limit+1 {
					t.Errorf("unknown body read %d bytes, want decoded bound plus detection byte %d", reader.read, limit+1)
				}
				if response.Header().Get(KeptBodyHeader) != "" || response.Header().Get(DictMissHeader) != "" {
					t.Errorf("oversize was acknowledged or treated as a replayable dictionary miss: %v", response.Header())
				}
				if heldDicts.get(fixture.scope, sum) != nil {
					t.Error("rejected payload was cached")
				}
				heldDicts.mu.Lock()
				after := heldDicts.size
				heldDicts.mu.Unlock()
				if after != kept {
					t.Errorf("rejected payload changed dictionary storage: %d -> %d", kept, after)
				}
				if fixture.decodes.Load() != decodes || fixture.sends.Load() != sends {
					t.Error("oversized request reached Decode or provider dispatch")
				}
				if !reader.closed {
					t.Error("rejected request body was not closed")
				}
			})
		}
	}
}

func TestDictionaryConfiguredBoundsApplyThroughFront(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", CodexPath + "/responses"} {
		for _, encoding := range []string{"identity", "unknown", "zstd", "dcz"} {
			t.Run(endpoint+"/"+encoding, func(t *testing.T) {
				const limit = 32 << 10
				fixture := newDictionaryBoundFixture(t, limit)
				var dictionary []byte
				var dictionarySum bodySum
				if encoding == "dcz" {
					dictionary, dictionarySum = fixture.seed(t)
				}
				reader, length, sum := dictionaryBoundInput(t, 2*limit, encoding, dictionary)
				if (encoding == "zstd" || encoding == "dcz") && length >= limit {
					t.Fatal("compressed fixture must fit the wire limit and exceed the decoded limit")
				}
				request := fixture.request(endpoint, reader, length)
				if encoding == "zstd" || encoding == "dcz" {
					request.Header.Set("Content-Encoding", encoding)
				}
				if encoding == "dcz" {
					request.Header.Set(availableDictHeader, sfBinary(dictionarySum))
					request.Header.Set(BodyDigestHeader, sfBinary(sum))
				}
				decodes, sends := fixture.decodes.Load(), fixture.sends.Load()
				response := httptest.NewRecorder()
				fixture.handler.ServeHTTP(response, request)
				if response.Code != http.StatusRequestEntityTooLarge || response.Header().Get(KeptBodyHeader) != "" || heldDicts.get(fixture.scope, sum) != nil {
					t.Errorf("configured bound bypass: %d %v", response.Code, response.Header())
				}
				if encoding == "identity" && reader.read != 0 || encoding == "unknown" && reader.read != limit+1 {
					t.Errorf("unbounded %s read %d", encoding, reader.read)
				}
				if fixture.decodes.Load() != decodes || fixture.sends.Load() != sends {
					t.Error("rejected request reached Decode or provider dispatch")
				}
				if response.Header().Get(DictMissHeader) != "" || !reader.closed {
					t.Error("rejected request invited replay or left its body open")
				}
			})
		}
	}
}

func TestDictionaryBelowBoundsRetainsOrdinaryAndNativeDispatch(t *testing.T) {
	for _, endpoint := range []string{"/fixture/rpc", "/v1/chat/completions", CodexPath + "/responses"} {
		for _, encoding := range []string{"identity", "unknown", "zstd", "dcz"} {
			t.Run(endpoint+"/"+encoding, func(t *testing.T) {
				const limit = 32 << 10
				fixture := newDictionaryBoundFixture(t, limit)
				payload := []byte(`{"model":"fixture/m1","messages":[{"role":"user","content":"hello"}],"input":"hello"}` + strings.Repeat(" ", dictMin))
				sum := sha256.Sum256(payload)
				var dictionary []byte
				var dictionarySum bodySum
				if encoding == "dcz" {
					dictionary, dictionarySum = fixture.seed(t)
				}
				wire := payload
				if encoding == "zstd" || encoding == "dcz" {
					var encoded bytes.Buffer
					options := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
					if encoding == "dcz" {
						encoded.Write(dczMagic)
						encoded.Write(dictionarySum[:])
						options = append(options, zstd.WithEncoderDictRaw(0, dictionary))
					}
					writer, err := zstd.NewWriter(&encoded, options...)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := writer.Write(payload); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					wire = encoded.Bytes()
				}
				reader := &dictionaryBoundCounter{source: bytes.NewReader(wire)}
				length := int64(len(wire))
				if encoding == "unknown" {
					length = -1
				}
				request := fixture.request(endpoint, reader, length)
				if encoding == "zstd" || encoding == "dcz" {
					request.Header.Set("Content-Encoding", encoding)
				}
				if encoding == "dcz" {
					request.Header.Del(KeepBodyHeader)
					request.Header.Set(availableDictHeader, sfBinary(dictionarySum))
					request.Header.Set(BodyDigestHeader, sfBinary(sum))
				}
				decodes, sends := fixture.decodes.Load(), fixture.sends.Load()
				response := httptest.NewRecorder()
				fixture.handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK || fixture.sends.Load() != sends+1 || !strings.Contains(response.Body.String(), "ok") {
					t.Fatalf("below-limit dispatch: %d sends=%d %s", response.Code, fixture.sends.Load(), response.Body)
				}
				if encoding != "dcz" && (response.Header().Get(KeptBodyHeader) != sfBinary(sum) || heldDicts.get(fixture.scope, sum) == nil) {
					t.Fatal("below-limit dictionary was not acknowledged and cached")
				}
				if encoding == "dcz" && (response.Header().Get(KeptBodyHeader) != "" || heldDicts.get(fixture.scope, sum) != nil) {
					t.Fatal("dictionary-only input was offered back or cached without an offer")
				}
				if reader.read != int64(len(wire)) {
					t.Fatalf("wire body read %d, want %d once", reader.read, len(wire))
				}
				wantDecodes := int64(0)
				if endpoint == "/fixture/rpc" {
					wantDecodes = 1
				}
				if fixture.decodes.Load() != decodes+wantDecodes {
					t.Fatalf("Decode count %d, want %d", fixture.decodes.Load(), decodes+wantDecodes)
				}
			})
		}
	}
}
