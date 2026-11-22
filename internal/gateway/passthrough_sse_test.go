package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

const passthroughMessages = `{"model":"fake/m1","stream":true,"max_tokens":8192,"messages":[{"role":"user","content":"hi"}]}`

const passthroughStart = "event: message_start\r\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\r\n\r\n"

const passthroughText = "event: content_block_delta\r\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\r\n" +
	"data: \"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\r\n\r\n"

const passthroughStop = "event: message_delta\r\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\r\n" +
	"data: \"usage\":{\"output_tokens\":7}}\r\n\r\n" +
	"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"

func TestPassthroughSSEFramingAndUsage(t *testing.T) {
	fresh(t)
	const first = ": vendor comment\r\nid: fixture-1\r\nretry: 3500\r\n\r\n" + passthroughStart + passthroughText
	const last = ": a second comment\n\n" + passthroughStop
	release := make(chan struct{})
	setup(t, provider.Anthropic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "fixture-upstream")
		io.WriteString(w, first+last[:len(last)/2])
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, last[len(last)/2:])
		case <-r.Context().Done():
		}
	}))
	gateway := New()
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(passthroughMessages))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Request-Id") != "fixture-upstream" {
		t.Fatalf("stream header: %d %v", res.StatusCode, res.Header)
	}
	start := make([]byte, len(first))
	if _, err := io.ReadFull(res.Body, start); err != nil {
		t.Fatalf("reading the first events while upstream held its tail: %v", err)
	}
	close(release)
	end, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(start) + string(end); got != first+last {
		t.Fatalf("stream = %q, want %q", got, first+last)
	}
	calls := gateway.Recent()
	if len(calls) != 1 || calls[0].Error != "" || calls[0].Usage.Input != 11 || calls[0].Usage.Output != 7 ||
		calls[0].Usage.ResponseID != "msg_fixture" || calls[0].Usage.Stop != "end_turn" {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestPassthroughSSECancellation(t *testing.T) {
	fresh(t)
	upstreamDone := make(chan struct{})
	setup(t, provider.Anthropic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, passthroughStart+passthroughText)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	server := httptest.NewServer(New().Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(passthroughMessages))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	first := make([]byte, len(passthroughStart+passthroughText))
	if _, err := io.ReadFull(res.Body, first); err != nil || string(first) != passthroughStart+passthroughText {
		t.Fatalf("first events: %q, %v", first, err)
	}
	cancel()
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request survived client cancellation")
	}
}

func BenchmarkPassthroughSSE(b *testing.B) {
	for _, protocol := range []provider.Protocol{provider.Anthropic, provider.Responses} {
		b.Run(string(protocol), func(b *testing.B) {
			b.Setenv("XDG_CONFIG_HOME", b.TempDir())
			b.Setenv("XDG_CACHE_HOME", b.TempDir())
			noRests()
			b.Cleanup(noRests)
			var stream strings.Builder
			path, request := "/v1/messages", passthroughMessages
			if protocol == provider.Responses {
				path = "/v1/responses"
				request = `{"model":"fake/m1","stream":true,"input":[{"role":"user","content":"hi"}]}`
				stream.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"in_progress\"}}\n\n")
			} else {
				stream.WriteString(passthroughStart)
			}
			for index := range 1024 {
				text := fmt.Sprintf("The compiler reads source file file_%04d.go before analyzing the imports and type declarations in package package_%04d. ", index, index*7)
				if protocol == provider.Responses {
					fmt.Fprintf(&stream, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":%q}\n\n", text)
				} else {
					fmt.Fprintf(&stream, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
				}
			}
			if protocol == provider.Responses {
				stream.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":32768}}}\n\n")
			} else {
				stream.WriteString(passthroughStop)
			}
			reply := stream.String()
			upstream := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, reply)
			})))
			defer upstream.Close()
			config := provider.Provider{ID: "fake", Name: "Fake", Key: "test-key", Models: []string{"m1"}}
			if protocol == provider.Responses {
				config.Responses = upstream.URL + "/v1"
			} else {
				config.Anthropic = upstream.URL
			}
			if err := provider.Save(config); err != nil {
				b.Fatal(err)
			}
			server := httptest.NewServer(New().Handler())
			defer server.Close()
			client := server.Client()
			client.Timeout = 20 * time.Second
			b.SetBytes(int64(len(reply)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				res, err := client.Post(server.URL+path, "application/json", strings.NewReader(request))
				if err != nil {
					b.Fatal(err)
				}
				n, err := io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != http.StatusOK || err != nil || n != int64(len(reply)) {
					b.Fatalf("stream: status %d, %d/%d bytes, %v", res.StatusCode, n, len(reply), err)
				}
			}
			b.StopTimer()
		})
	}
}
