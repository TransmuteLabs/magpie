package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/protocolext"
)

type dictionaryDeadlineReader struct {
	io.ReadCloser
	firstRead time.Time
}

func (reader *dictionaryDeadlineReader) Read(buffer []byte) (int, error) {
	if reader.firstRead.IsZero() {
		reader.firstRead = time.Now()
	}
	return reader.ReadCloser.Read(buffer)
}

func TestNativeDictionaryDeadlineIncludesInputRead(t *testing.T) {
	for _, scenario := range []struct {
		name, encoding string
		offered        bool
		timeout        time.Duration
		parentTimeout  time.Duration
	}{
		{name: "ordinary", encoding: "identity", timeout: 10 * time.Second, parentTimeout: time.Hour},
		{name: "offered", encoding: "identity", offered: true, timeout: 10 * time.Second, parentTimeout: time.Hour},
		{name: "offered-unknown", encoding: "unknown", offered: true, timeout: 10 * time.Second, parentTimeout: time.Hour},
		{name: "offered-zstd", encoding: "zstd", offered: true, timeout: 10 * time.Second, parentTimeout: time.Hour},
		{name: "dcz", encoding: "dcz", timeout: 10 * time.Second, parentTimeout: time.Hour},
		{name: "earlier-parent", encoding: "identity", offered: true, timeout: 10 * time.Second, parentTimeout: 5 * time.Second},
		{name: "zero-timeout", encoding: "identity", offered: true, parentTimeout: time.Hour},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fresh(t)
			freshDicts(t)
			keys, secrets := newCaller(t, "Native deadline")
			if err := provider.Save(provider.Provider{
				ID: "fixture", Name: "Fixture", Key: "fixture-key",
				Chat: "http://dictionary-deadline.invalid/v1", Models: []string{"m1"},
			}); err != nil {
				t.Fatal(err)
			}
			const canonical = `{"model":"fixture/m1","messages":[{"role":"user","content":"hello"}]}`
			decodes, sends, finishes := 0, 0, 0
			var deadline time.Time
			var hasDeadline bool
			server := protocolServer(t, fixtureInbound{
				decode: func(ctx context.Context, headers http.Header, body []byte) (sdk.Request, error) {
					decodes++
					if access.Caller(ctx).KeyID != keys[0].ID || len(body) != dictMin+101 {
						t.Error("Decode lost authenticated caller or decoded body")
					}
					for _, name := range []string{KeepBodyHeader, availableDictHeader, BodyDigestHeader, "Content-Encoding"} {
						if headers.Get(name) != "" {
							t.Errorf("dictionary header reached Decode: %s", name)
						}
					}
					return sdk.Request{Protocol: sdk.Chat, Body: []byte(canonical), Timeout: scenario.timeout}, nil
				},
				finish: func(cause error) error {
					finishes++
					if cause != nil {
						t.Errorf("successful request Finish cause: %v", cause)
					}
					return nil
				},
			})
			server.client = &http.Client{Transport: countTransport(func(request *http.Request) (*http.Response, error) {
				sends++
				deadline, hasDeadline = request.Context().Deadline()
				body, err := io.ReadAll(request.Body)
				if err != nil || !bytes.Contains(body, []byte(`"hello"`)) ||
					request.Method != http.MethodPost || request.URL.String() != "http://dictionary-deadline.invalid/v1/chat/completions" {
					t.Errorf("unexpected canonical dispatch: %s %s body=%s err=%v", request.Method, request.URL, body, err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
					Request:    request,
				}, nil
			})}
			server.requestLimits.body = 32 << 10
			handler := server.Front()
			makeRequest := func(body io.ReadCloser, length int64) *http.Request {
				request := httptest.NewRequest(http.MethodPost, "/fixture/rpc", nil)
				request.Body, request.ContentLength = body, length
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set("Authorization", "Bearer "+secrets[0])
				request.Header.Set("User-Agent", "magpie/0.1.9999")
				return request
			}
			var dictionary []byte
			var dictionarySum bodySum
			if scenario.encoding == "dcz" {
				dictionary = bytes.Repeat([]byte("d"), dictMin+101)
				dictionarySum = sha256.Sum256(dictionary)
				seed := makeRequest(io.NopCloser(bytes.NewReader(dictionary)), int64(len(dictionary)))
				seed.Header.Set(KeepBodyHeader, "dcz")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, seed)
				if response.Code != http.StatusOK || response.Header().Get(KeptBodyHeader) != sfBinary(dictionarySum) ||
					heldDicts.get(keys[0].ID+"\x00127.0.0.1", dictionarySum) == nil || decodes != 1 || sends != 1 || finishes != 1 {
					t.Fatalf("actual same-scope dictionary seed failed: %d %v %s", response.Code, response.Header(), response.Body)
				}
				decodes, sends, finishes = 0, 0, 0
				deadline, hasDeadline = time.Time{}, false
			}
			input, length, sum := dictionaryBoundInput(t, dictMin+101, scenario.encoding, dictionary)
			reader := &dictionaryDeadlineReader{ReadCloser: input}
			request := makeRequest(reader, length)
			if scenario.offered {
				request.Header.Set(KeepBodyHeader, "dcz")
			}
			if scenario.encoding == "zstd" || scenario.encoding == "dcz" {
				request.Header.Set("Content-Encoding", scenario.encoding)
			}
			if scenario.encoding == "dcz" {
				request.Header.Set(availableDictHeader, sfBinary(dictionarySum))
				request.Header.Set(BodyDigestHeader, sfBinary(sum))
			}
			ctx, cancel := context.WithTimeout(context.Background(), scenario.parentTimeout)
			defer cancel()
			parentDeadline, _ := ctx.Deadline()
			request = request.WithContext(ctx)
			response := httptest.NewRecorder()
			entered := time.Now()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ok") ||
				decodes != 1 || sends != 1 || finishes != 1 {
				t.Fatalf("dispatch: status=%d Decode=%d sends=%d Finish=%d body=%s", response.Code, decodes, sends, finishes, response.Body)
			}
			if reader.firstRead.IsZero() || !hasDeadline {
				t.Fatal("first input read or outgoing deadline was not recorded")
			}
			if scenario.timeout == 0 || scenario.parentTimeout < scenario.timeout {
				if !deadline.Equal(parentDeadline) {
					t.Errorf("parent deadline changed: got %v want %v", deadline, parentDeadline)
				}
			} else {
				if !parentDeadline.After(reader.firstRead.Add(scenario.timeout)) {
					t.Fatal("fixture parent deadline hid the declared timeout")
				}
				if deadline.Before(entered.Add(scenario.timeout)) {
					t.Errorf("unrelated earlier deadline hid the declared timeout: %v", deadline)
				}
				if deadline.After(reader.firstRead.Add(scenario.timeout)) {
					t.Errorf("input read time excluded: deadline=%v firstRead+timeout=%v excess=%v", deadline, reader.firstRead.Add(scenario.timeout), deadline.Sub(reader.firstRead.Add(scenario.timeout)))
				}
			}
			if length >= 0 && input.read != length || length < 0 && input.read != dictMin+101 {
				t.Errorf("wire body was not read exactly once: %d length=%d", input.read, length)
			}
			if scenario.offered {
				if response.Header().Get(KeptBodyHeader) != sfBinary(sum) || heldDicts.get(keys[0].ID+"\x00127.0.0.1", sum) == nil {
					t.Error("successful offered body lost its same-scope cache/ack")
				}
			} else if response.Header().Get(KeptBodyHeader) != "" {
				t.Error("body without an offer was acknowledged as kept")
			}
		})
	}
}
