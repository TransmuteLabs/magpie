package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"golang.org/x/sys/unix"
)

const (
	responsesTCPBuffer      = 8 << 10
	responsesTCPBacklogTail = "The synthetic TCP backlog payload ends here."
)

type responsesTCPListener struct {
	net.Listener
	accepted chan<- *net.TCPConn
	writes   chan<- responsesTCPWrite
}

func (listener responsesTCPListener) Accept() (net.Conn, error) {
	conn, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp := conn.(*net.TCPConn)
	select {
	case listener.accepted <- tcp:
	default:
	}
	return &responsesTCPConn{TCPConn: tcp, writes: listener.writes}, nil
}

type responsesTCPConn struct {
	*net.TCPConn
	writes  chan<- responsesTCPWrite
	written atomic.Uint64
}

type responsesTCPWrite struct {
	size       int
	before     uint64
	typePrefix bool
	done       <-chan responsesTCPWriteResult
}

type responsesTCPWriteResult struct {
	count int
	err   error
}

func (conn *responsesTCPConn) Write(data []byte) (int, error) {
	var done chan responsesTCPWriteResult
	if len(data) >= 128<<10 && bytes.Contains(data, []byte(responsesTCPBacklogTail)) {
		done = make(chan responsesTCPWriteResult, 1)
		pending := responsesTCPWrite{
			size:       len(data),
			before:     conn.written.Load(),
			typePrefix: bytes.Contains(data, []byte(`"type":"response.output_text.delta"`)),
			done:       done,
		}
		select {
		case conn.writes <- pending:
		default:
		}
	}
	count, err := conn.TCPConn.Write(data)
	conn.written.Add(uint64(count))
	if done != nil {
		done <- responsesTCPWriteResult{count: count, err: err}
	}
	return count, err
}

func responsesTCPStatus(t *testing.T, socket syscall.RawConn) (*unix.TCPInfo, int16) {
	t.Helper()
	var info *unix.TCPInfo
	var events int16
	var probeErr error
	controlErr := socket.Control(func(fd uintptr) {
		poll := [1]unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT | unix.POLLRDHUP}}
		for {
			_, probeErr = unix.Poll(poll[:], 0)
			if probeErr != unix.EINTR {
				break
			}
		}
		if probeErr == nil {
			events = poll[0].Revents
			info, probeErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		}
	})
	if err := errors.Join(controlErr, probeErr); err != nil {
		t.Fatalf("read socket state: %v", err)
	}
	return info, events
}

func TestResponsesSocketOverflowWhileTCPWriteBlocked(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	const session = "responses-tcp-overflow"
	const requestID = "req-tcp-overflow"
	sendBacklog := make(chan struct{})
	backlogFlushed := make(chan struct{})
	upstreamRequests := make(chan context.Context, 1)
	up := &respUp{}
	up.reply = func(writer http.ResponseWriter, request *http.Request, ordinal int) {
		if ordinal != 1 {
			t.Errorf("queued turn reached the vendor: request %d", ordinal)
			http.Error(writer, "unexpected vendor request", http.StatusInternalServerError)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
			t.Errorf("vendor got %s %s", request.Method, request.URL.Path)
		}
		upstreamRequests <- request.Context()
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("X-Request-Id", requestID)
		controller := http.NewResponseController(writer)
		_, err := io.WriteString(writer, sse(
			`data: {"type":"response.created","response":{"id":"resp_wait","object":"response","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_wait","role":"assistant","status":"in_progress","content":[]}}`,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_wait","delta":"partial"}`,
		))
		if err := errors.Join(err, controller.Flush()); err != nil {
			t.Errorf("sending first vendor events: %v", err)
			return
		}
		select {
		case <-sendBacklog:
		case <-request.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		var delta strings.Builder
		for index := 0; delta.Len() < 256<<10; index++ {
			fmt.Fprintf(&delta, "Synthetic response row %05d carries a distinct text record.\n", index)
		}
		delta.WriteString(responsesTCPBacklogTail)
		_, err = io.WriteString(writer, sse(
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_wait","delta":`+
				strconv.Quote(delta.String())+`}`,
		))
		if err := errors.Join(err, controller.Flush()); err != nil {
			t.Errorf("sending the vendor backlog: %v", err)
			return
		}
		close(backlogFlushed)
		select {
		case <-request.Context().Done():
		case <-ctx.Done():
		}
	}
	sockUp(t, up)

	keys, secrets := newCaller(t, "Responses socket caller")
	accepted := make(chan *net.TCPConn, 1)
	writes := make(chan responsesTCPWrite, 1)
	served := make(chan struct{})
	front := New().Front()
	public := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer close(served)
		request.RemoteAddr = "192.168.1.9:5000"
		front.ServeHTTP(writer, request)
	}))
	public.Listener = responsesTCPListener{Listener: public.Listener, accepted: accepted, writes: writes}
	public.Start()
	defer public.Close()

	dialer := net.Dialer{Control: func(_, _ string, socket syscall.RawConn) error {
		var optionErr error
		controlErr := socket.Control(func(fd uintptr) {
			optionErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, responsesTCPBuffer)
		})
		return errors.Join(controlErr, optionErr)
	}}
	transport := &http.Transport{DialContext: dialer.DialContext}
	defer transport.CloseIdleConnections()
	client, _, err := websocket.Dial(
		ctx,
		"ws"+strings.TrimPrefix(public.URL, "http")+"/v1/responses",
		&websocket.DialOptions{
			HTTPClient:      &http.Client{Transport: transport},
			CompressionMode: websocket.CompressionDisabled,
			HTTPHeader: http.Header{
				"Authorization": {"Bearer " + secrets[0]},
				SessionHeader:   {session},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.CloseNow()
		select {
		case <-served:
		case <-time.After(6 * time.Second):
			t.Error("public WebSocket handler did not exit after cleanup")
		}
	}()

	var serverConn *net.TCPConn
	select {
	case serverConn = <-accepted:
	case <-ctx.Done():
		t.Fatal("public TCP socket was not accepted")
	}
	defer serverConn.Close()
	socket, err := serverConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := serverConn.SetWriteBuffer(responsesTCPBuffer); err != nil {
		t.Fatal(err)
	}
	if err := client.Write(ctx, websocket.MessageText, []byte(create("", userHi))); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"response.created", "response.output_item.added", "response.output_text.delta"} {
		messageType, data, err := client.Read(ctx)
		event := gjson.ParseBytes(data)
		if err != nil || messageType != websocket.MessageText || event.Get("type").String() != kind {
			t.Fatalf("expected %s, got %s (%v)", kind, event.Get("type"), err)
		}
		if kind == "response.created" && event.Get("response.id").String() != "resp_wait" {
			t.Fatal("the first frame did not identify the upstream response")
		}
		if kind == "response.output_text.delta" && event.Get("delta").String() != "partial" {
			t.Fatal("the first useful delta did not reach the client")
		}
	}
	var upstreamCtx context.Context
	select {
	case upstreamCtx = <-upstreamRequests:
	case <-ctx.Done():
		t.Fatal("the upstream request did not start")
	}
	before, _ := responsesTCPStatus(t, socket)
	if before.Bytes_sent == 0 {
		t.Fatal("TCP_INFO reported no sent bytes after the client read the first frame")
	}

	close(sendBacklog)
	select {
	case <-backlogFlushed:
	case <-upstreamCtx.Done():
		t.Fatal("upstream ended before flushing the backlog")
	case <-ctx.Done():
		t.Fatal("the vendor backlog was not flushed")
	}
	var pending responsesTCPWrite
	select {
	case pending = <-writes:
	case <-upstreamCtx.Done():
		t.Fatal("upstream ended before a large downstream write")
	case <-ctx.Done():
		t.Fatal("the large WebSocket payload did not reach TCP")
	}
	if pending.before == 0 {
		t.Fatal("the payload write has no preceding socket bytes")
	}
	if pending.typePrefix {
		t.Fatal("the large payload write still contains the WebSocket event type prefix")
	}
	sample := time.NewTicker(5 * time.Millisecond)
	defer sample.Stop()
	var acceptedAtLeast uint64
	for {
		info, ready := responsesTCPStatus(t, socket)
		closedWindow := before.Snd_wnd > 0 && info.Snd_wnd == 0
		windowStall := info.Rwnd_limited - min(info.Rwnd_limited, before.Rwnd_limited)
		admittedAtMost := info.Bytes_sent + uint64(info.Notsent_bytes)
		admittedAtLeast := admittedAtMost - min(info.Bytes_sent, info.Bytes_retrans)
		if ready == 0 && (closedWindow || windowStall > 0) &&
			info.Bytes_sent > before.Bytes_sent && info.Notsent_bytes > 0 &&
			admittedAtLeast > pending.before && admittedAtMost-pending.before < uint64(pending.size) {
			acceptedAtLeast = admittedAtLeast - pending.before
			t.Logf(
				"TCP backpressure: type-prefix=%t poll=%#x window=%d initial-window=%d before-sent=%d before-write=%d sent=%d retrans=%d unsent=%d write=%d rwnd-limited-us=%d",
				pending.typePrefix,
				ready,
				info.Snd_wnd,
				before.Snd_wnd,
				before.Bytes_sent,
				pending.before,
				info.Bytes_sent,
				info.Bytes_retrans,
				info.Notsent_bytes,
				pending.size,
				windowStall,
			)
			break
		}
		select {
		case result := <-pending.done:
			t.Fatalf("TCP write returned before backpressure: %d/%d, %v", result.count, pending.size, result.err)
		case <-upstreamCtx.Done():
			t.Fatal("upstream ended before TCP backpressure")
		case <-ctx.Done():
			t.Fatalf("no TCP backpressure: poll=%#x, window=%d, sent=%d, retrans=%d, unsent=%d, before=%d, write=%d",
				ready, info.Snd_wnd, info.Bytes_sent, info.Bytes_retrans, info.Notsent_bytes, pending.before, pending.size)
		case <-sample.C:
		}
	}
	select {
	case result := <-pending.done:
		t.Fatalf("TCP write returned before overflow: %d/%d, %v", result.count, pending.size, result.err)
	case <-upstreamCtx.Done():
		t.Fatal("upstream ended before overflow")
	default:
	}

	overflow, stopWaiting := context.WithTimeout(ctx, 3*time.Second)
	defer stopWaiting()
	queued := []byte(create("", userMore))
	for range socketQueueFrames + 1 {
		if err := client.Write(overflow, websocket.MessageText, queued); err != nil {
			t.Fatalf("send queued turn: %v", err)
		}
	}
	select {
	case <-upstreamCtx.Done():
	case <-overflow.Done():
		t.Fatal("overflow did not cancel the vendor with an unread peer")
	}
	select {
	case result := <-pending.done:
		if result.err == nil || result.count < int(acceptedAtLeast) || result.count >= pending.size {
			t.Fatalf("overflow did not abort the pending TCP write: %d/%d, %v", result.count, pending.size, result.err)
		}
		t.Logf("overflow aborted TCP write: %d/%d", result.count, pending.size)
	case <-overflow.Done():
		t.Fatal("overflow did not release the pending TCP write")
	}
	select {
	case <-served:
	case <-overflow.Done():
		t.Fatal("overflow did not release the WebSocket handler with an unread peer")
	}
	if err := socket.Control(func(uintptr) {}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the server TCP descriptor remains open after overflow: %v", err)
	}
	if err := overflow.Err(); err != nil {
		t.Fatalf("overflow cleanup exceeded its deadline: %v", err)
	}
	checkResponsesSocketNativeUsage(t, up, 1, keys[0].ID, session, requestID)
}
