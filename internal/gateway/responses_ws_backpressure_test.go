package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/usage"
)

const responsesHeldDelta = "held responses socket delta: the reader returns after the vendor pauses"

type responsesWriteGate struct {
	net.Listener
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (gate *responsesWriteGate) Accept() (net.Conn, error) {
	conn, err := gate.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &responsesGatedConn{Conn: conn, gate: gate}, nil
}

type responsesGatedConn struct {
	net.Conn
	gate *responsesWriteGate
}

func (conn *responsesGatedConn) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte(responsesHeldDelta)) {
		conn.gate.once.Do(func() { close(conn.gate.entered) })
		<-conn.gate.release
	}
	return conn.Conn.Write(data)
}

func TestResponsesSocketNativeSSECancelWhileWriteBlocked(t *testing.T) {
	for _, mode := range []struct {
		name       string
		disconnect bool
	}{{"interrupt", false}, {"disconnect", true}} {
		t.Run(mode.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "")
			t.Setenv("MAGPIE_PUBLIC_URL", "")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			waitFor := func(done <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-done:
					if ctx.Err() == nil {
						return
					}
				case <-ctx.Done():
				}
				t.Fatalf("timed out waiting for %s", what)
			}

			const session = "native-responses-slow-reader"
			const requestID = "req-without-terminal"
			sendDelta, deltaFlushed, upstreamCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var upstreamContext context.Context
			up := &respUp{}
			up.reply = func(writer http.ResponseWriter, request *http.Request, _ int) {
				up.mu.Lock()
				up.reply = nil
				up.mu.Unlock()
				if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
					t.Errorf("vendor got %s %s", request.Method, request.URL.Path)
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("X-Request-Id", requestID)
				flush := http.NewResponseController(writer).Flush
				_, err := io.WriteString(writer, sse(
					`data: {"type":"response.created","response":{"id":"resp_wait","object":"response","status":"in_progress","output":[]}}`,
					`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_wait","role":"assistant","status":"in_progress","content":[]}}`,
					`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_wait","delta":"partial"}`))
				if err := errors.Join(err, flush()); err != nil {
					t.Errorf("sending first vendor events: %v", err)
					return
				}
				select {
				case <-sendDelta:
				case <-request.Context().Done():
					return
				case <-ctx.Done():
					return
				}
				_, err = io.WriteString(writer, sse(
					`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_wait","delta":"`+responsesHeldDelta+`"}`))
				if err := errors.Join(err, flush()); err != nil {
					t.Errorf("sending held vendor event: %v", err)
					return
				}
				upstreamContext = request.Context()
				close(deltaFlushed)
				select {
				case <-upstreamContext.Done():
					close(upstreamCanceled)
				case <-ctx.Done():
				}
			}
			sockUp(t, up)
			keys, secrets := newCaller(t, "Responses socket caller")
			front := New().Front()
			served := make(chan struct{})
			public := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer close(served)
				request.RemoteAddr = "192.168.1.9:5000"
				front.ServeHTTP(writer, request)
			}))
			release := make(chan struct{})
			resume := sync.OnceFunc(func() { close(release) })
			gate := &responsesWriteGate{Listener: public.Listener, entered: make(chan struct{}), release: release}
			public.Listener = gate
			public.Start()
			defer public.Close()
			client, _, err := dialSock(t, public, "/v1/responses", http.Header{
				"Authorization": {"Bearer " + secrets[0]},
				SessionHeader:   {session},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				resume()
				_ = client.c.CloseNow()
				select {
				case <-served:
				case <-time.After(10 * time.Second):
					t.Error("the public socket handler did not exit")
				}
			}()

			client.send(create("", userHi))
			start, err := client.until("response.output_text.delta", "response.completed", "response.failed", "response.incomplete", "error")
			if err != nil || len(start) != 3 || start[0].Get("type").String() != "response.created" ||
				start[0].Get("response.id").String() != "resp_wait" ||
				start[1].Get("type").String() != "response.output_item.added" ||
				start[2].Get("type").String() != "response.output_text.delta" ||
				start[2].Get("delta").String() != "partial" {
				t.Fatalf("early Responses frames: %v, %v", start, err)
			}
			close(sendDelta)
			waitFor(deltaFlushed, "the vendor's second delta flush")
			waitFor(gate.entered, "the held WebSocket write")
			if err := upstreamContext.Err(); err != nil {
				t.Fatalf("vendor request ended before client %s: %v", mode.name, err)
			}

			if mode.disconnect {
				if err := client.c.CloseNow(); err != nil {
					t.Fatal(err)
				}
			} else {
				client.send(`{"type":"response.interrupt","response_id":"resp_wait","mode":"discard_partial_items"}`)
			}
			waitFor(upstreamCanceled, "upstream cancellation while the WebSocket write is held")
			resume()

			wantCalls := 1
			if !mode.disconnect {
				events, err := client.until("response.incomplete", "response.interrupt.failed", "response.completed", "response.failed", "error")
				if err != nil || len(events) != 3 || events[0].Get("type").String() != "response.output_text.delta" ||
					events[0].Get("item_id").String() != "msg_wait" || events[0].Get("delta").String() != responsesHeldDelta ||
					events[1].Get("type").String() != "response.interrupt.accepted" ||
					events[1].Get("response_id").String() != "resp_wait" ||
					events[2].Get("type").String() != "response.incomplete" ||
					events[2].Get("response.id").String() != "resp_wait" ||
					events[2].Get("response.incomplete_details.reason").String() != "interrupted" ||
					events[2].Get("response.output").Raw != "[]" {
					t.Fatalf("resumed socket got %d interrupt frames, error %v", len(events), err)
				}
				followup := client.turn(create("", userMore))
				end := last(followup)
				if len(followup) != 3 || followup[0].Get("type").String() != "response.created" ||
					followup[1].Get("type").String() != "response.output_item.done" ||
					end.Get("type").String() != "response.completed" || end.Get("response.id").String() != "resp_up2" ||
					end.Get("response.output.0.content.0.text").String() != "answer 2" ||
					end.Get("response.usage.input_tokens").Int() != 300 ||
					end.Get("response.usage.output_tokens").Int() != 50 ||
					end.Get("response.usage.total_tokens").Int() != 350 {
					t.Fatalf("same-socket follow-up: %v", followup)
				}
				wantCalls++
				_ = client.c.CloseNow()
			}
			waitFor(served, "the public WebSocket handler to join the turn")
			checkResponsesSocketNativeUsage(t, up, wantCalls, keys[0].ID, session, requestID)
		})
	}
}

func checkResponsesSocketNativeUsage(t *testing.T, up *respUp, wantCalls int, caller, session, requestID string) {
	t.Helper()
	up.mu.Lock()
	bodies, headers := up.bodies, up.head
	up.mu.Unlock()
	if len(bodies) != wantCalls || len(headers) != wantCalls {
		t.Fatalf("vendor requests = %d, want %d without replay", len(bodies), wantCalls)
	}
	prompts := []string{"hi", "and more"}
	for index, raw := range bodies {
		body := gjson.ParseBytes(raw)
		if headers[index].Get("Authorization") != "Bearer k" || body.Get("model").String() != "m1" ||
			body.Get("stream").Type != gjson.True || body.Get("input.0.content.0.text").String() != prompts[index] {
			t.Fatalf("vendor request %d did not keep its credential, model, stream or input", index+1)
		}
	}

	rows := usage.Load(time.Time{})
	if len(rows) != wantCalls {
		t.Fatalf("persisted Usage rows = %d, want %d", len(rows), wantCalls)
	}
	for _, row := range rows {
		if row.CallerKeyID != caller || row.Session != session || row.Provider != "fake" ||
			row.Model != "m1" || row.Endpoint != "/v1/responses" {
			t.Errorf("persisted Usage lost caller, session or route: %+v", row)
		}
	}
	canceled := rows[0]
	if canceled.Status != usage.StatusCanceled || canceled.Error != usage.AgentCanceled ||
		canceled.ErrType != usage.CanceledErrType || canceled.Input != 0 ||
		canceled.Output != 0 || canceled.RequestID != requestID {
		t.Errorf("unfinished native turn Usage: %+v", canceled)
	}
	if wantCalls == 2 {
		completed := rows[1]
		if completed.Status != http.StatusOK || completed.Error != "" || completed.ResponseID != "resp_up2" ||
			completed.Input != 300 || completed.Output != 50 {
			t.Errorf("completed native turn Usage: %+v", completed)
		}
	}
}
