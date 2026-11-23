package gateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
)

const m09WebSocketSession = "m09-websocket"
const m09WebSocketDelta = "m09 socket text"

var m09WebSocketNativeChild = flag.Bool("m09-websocket-native-child", false, "run the native M09 WebSocket fixture in this test process")

func TestM09SDKWebSocketInterrupt(t *testing.T) {
	if !*m09RuntimeParity {
		t.Skip("enable with -args -m09-runtime-parity")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("M09 fixture supports Darwin and Linux native loaders")
	}
	for _, kind := range []string{"exec", "native"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "native" && !*m09WebSocketNativeChild {
				ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
				defer cancel()
				command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestM09SDKWebSocketInterrupt$/^native$",
					"-test.v", "-test.timeout=140s", "-m09-runtime-parity", "-m09-websocket-native-child")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("isolated native WebSocket case: %v\n%s", err, output)
				}
				prefix := fmt.Sprintf("M09 WS native host=%d parent=%d ", command.Process.Pid, os.Getpid())
				proof := ""
				for line := range strings.SplitSeq(string(output), "\n") {
					if _, receipt, found := strings.Cut(line, ": "+prefix); found {
						if proof != "" {
							t.Fatalf("duplicate native WebSocket receipt:\n%s", output)
						}
						proof = prefix + receipt
					}
				}
				if proof == "" {
					t.Fatalf("missing native WebSocket receipt for process %d:\n%s", command.Process.Pid, output)
				}
				t.Log(proof)
				return
			}
			testM09WebSocketInterrupt(t, kind)
		})
	}
}

func testM09WebSocketInterrupt(t *testing.T, kind string) {
	t.Helper()
	canceled := make(chan struct{})
	upstream := &respUp{reply: func(writer http.ResponseWriter, request *http.Request, number int) {
		serveM09WebSocketTurn(t, writer, request, number, canceled)
	}}
	vendor := testenv.FakeVendor(t, upstream)
	directory := buildM09Module(t, kind)
	manager, account := installM09Module(t, directory, vendor.URL, os.Getenv("FAKE_RUN"))
	id := provider.ExtensionProviderID(account.Plugin, account.Provider)
	keys, secrets := newCaller(t, "M09 WebSocket fixture")
	server, public := sockServer(t)
	t.Cleanup(server.idle.stop)
	client, _, err := dialSock(t, public, "/v1/responses", http.Header{
		"X-Test-Remote": {"192.168.1.9:5000"},
		"Authorization": {"Bearer " + secrets[0]},
		SessionHeader:   {m09WebSocketSession},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(index int) string {
		return fmt.Sprintf(`{"type":"response.create","model":%q,"input":%q}`, id+"/m1", m09Prompt(index))
	}
	client.send(turn(0))
	start, err := client.until("response.output_text.delta", "response.completed", "response.failed", "error")
	if err != nil || len(start) != 2 || start[0].Get("type").String() != "response.created" ||
		start[0].Get("response.id").String() != m09ResponseID(0) ||
		start[0].Get("response.model").String() != id+"/m1" ||
		last(start).Get("type").String() != "response.output_text.delta" || last(start).Get("delta").String() != m09WebSocketDelta {
		t.Fatalf("partial WebSocket reply: %v (%v)", start, err)
	}
	client.send(fmt.Sprintf(`{"type":"response.interrupt","response_id":%q,"mode":"discard_partial_items"}`, m09ResponseID(0)))
	interrupted, err := client.until("response.incomplete", "response.interrupt.failed", "response.completed", "response.failed", "error")
	if err != nil || len(interrupted) != 2 || interrupted[0].Get("type").String() != "response.interrupt.accepted" ||
		interrupted[0].Get("response_id").String() != m09ResponseID(0) ||
		last(interrupted).Get("type").String() != "response.incomplete" || last(interrupted).Get("response.id").String() != m09ResponseID(0) ||
		last(interrupted).Get("response.incomplete_details.reason").String() != "interrupted" ||
		last(interrupted).Get("response.output").Raw != "[]" {
		t.Fatalf("interrupted WebSocket reply: %v (%v)", interrupted, err)
	}
	cancelCtx, stopCancelWait := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopCancelWait()
	select {
	case <-canceled:
	case <-cancelCtx.Done():
		t.Fatal("interruption did not cancel the upstream SDK request")
	}
	followup := client.turn(turn(1))
	end := last(followup)
	if len(followup) != 3 || followup[0].Get("type").String() != "response.created" ||
		followup[0].Get("response.id").String() != m09ResponseID(1) ||
		followup[1].Get("type").String() != "response.output_text.delta" || followup[1].Get("delta").String() != m09WebSocketDelta ||
		end.Get("type").String() != "response.completed" || end.Get("response.id").String() != m09ResponseID(1) ||
		end.Get("response.model").String() != id+"/m1" || end.Get("response.output.0.content.0.text").String() != m09WebSocketDelta ||
		end.Get("response.usage.input_tokens").Int() != 16 || end.Get("response.usage.output_tokens").Int() != 32 ||
		end.Get("response.usage.total_tokens").Int() != 48 {
		t.Fatalf("same-socket follow-up: %v", followup)
	}
	drainCtx, stopDrain := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopDrain()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		server.sockets.drain(drainCtx)
	}()
	_, trailing, err := client.c.Read(drainCtx)
	<-drained
	if drainCtx.Err() != nil || websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("WebSocket completion did not drain cleanly: %s (%v)", trailing, err)
	}
	upstream.mu.Lock()
	bodies, headers := upstream.bodies, upstream.head
	upstream.mu.Unlock()
	if len(bodies) != 2 || len(headers) != 2 {
		t.Fatalf("upstream requests = %d, want exactly two without replay", len(bodies))
	}
	operations := [2]string{}
	workers := [2]int{}
	for index, state := range []string{"uncertain", "completed"} {
		body, head := bodies[index], headers[index]
		if gjson.GetBytes(body, "model").String() != "m1" || gjson.GetBytes(body, "stream").Type != gjson.True ||
			gjson.GetBytes(body, "input.0.content").String() != m09Prompt(index) ||
			head.Get("Authorization") != "" {
			t.Fatalf("turn %d reached upstream with incorrect model, input or headers", index)
		}
		workerPID, pidErr := strconv.Atoi(head.Get("X-Fixture-Pid"))
		if pidErr != nil || workerPID <= 0 ||
			(kind == "exec" && (workerPID == os.Getpid() || head.Get("X-Fixture-Parent-Pid") != strconv.Itoa(os.Getpid()))) ||
			(kind == "native" && (workerPID != os.Getpid() || head.Get("X-Fixture-Parent-Pid") != strconv.Itoa(os.Getppid()))) {
			t.Fatalf("turn %d reached the wrong %s worker: pid=%d host=%d", index, kind, workerPID, os.Getpid())
		}
		workers[index] = workerPID
		operations[index] = head.Get("X-Fixture-Operation")
		entry, err := manager.Store().OperationContext(t.Context(), account.Plugin, operations[index])
		if operations[index] == "" || err != nil || entry.State != state || entry.Scope.Operation != operations[index] ||
			entry.Scope.Plugin != account.Plugin || entry.Scope.Provider != account.Provider ||
			entry.Scope.Account != account.ID || entry.Scope.Generation != account.Generation {
			t.Fatalf("turn %d receipt = %+v (%v), want %s for selected account", index, entry, err, state)
		}
	}
	if operations[0] == operations[1] {
		t.Fatal("both WebSocket turns reused the same operation")
	}
	canceledRows := checkM09WebSocketUsage(t, id, account.ID, keys[0].ID)
	if !t.Failed() {
		t.Logf("M09 WS %s host=%d parent=%d workers=%d,%d vendor=2 interrupted=1 completed=1 successful_rows=1 canceled_rows=%d canceled_status=%d canceled_billing=0",
			kind, os.Getpid(), os.Getppid(), workers[0], workers[1], canceledRows, canceledRows*http.StatusConflict)
	}
}

func serveM09WebSocketTurn(t *testing.T, writer http.ResponseWriter, request *http.Request, number int, canceled chan<- struct{}) {
	t.Helper()
	if number > 2 || request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
		t.Error("unexpected M09 WebSocket vendor request")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	id := m09ResponseID(number - 1)
	writer.Header().Set("Content-Type", "text/event-stream")
	created := fmt.Sprintf(`data: {"type":"response.created","response":{"id":%q,"model":"m1","status":"in_progress","output":[]}}`, id)
	delta := fmt.Sprintf(`data: {"type":"response.output_text.delta","item_id":"item_m09","output_index":0,"content_index":0,"delta":%q}`, m09WebSocketDelta)
	_, err := io.WriteString(writer, sse(created, delta))
	if err := errors.Join(err, http.NewResponseController(writer).Flush()); err != nil {
		t.Errorf("M09 WebSocket vendor partial response: %v", err)
		return
	}
	if number == 1 {
		limit := time.NewTimer(15 * time.Second)
		defer limit.Stop()
		select {
		case <-request.Context().Done():
			close(canceled)
		case <-limit.C:
			t.Error("M09 WebSocket interrupted upstream remained open")
		}
		return
	}
	end := fmt.Sprintf(`data: {"type":"response.completed","response":{"id":%q,"model":"m1","status":"completed",`+
		`"output":[{"type":"message","id":"item_m09","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}],`+
		`"usage":{"input_tokens":16,"output_tokens":32,"total_tokens":48}}}`, id, m09WebSocketDelta)
	if _, err := io.WriteString(writer, sse(end)); err != nil {
		t.Errorf("M09 WebSocket vendor terminal response: %v", err)
	}
}

func checkM09WebSocketUsage(t *testing.T, id, accountID, callerID string) int {
	t.Helper()
	var completed, canceled int
	usage.Visit(time.Time{}, func(row usage.Record) {
		if row.Provider != id && row.CallerKeyID != callerID {
			return
		}
		if row.Provider != id || row.CallerKeyID != callerID || row.ProviderAccount != accountID ||
			row.Model != "m1" || row.Session != m09WebSocketSession {
			t.Errorf("WebSocket Usage lost its caller, account, model or session: %+v", row)
		}
		switch row.Status {
		case http.StatusOK:
			completed++
			if row.ResponseID != m09ResponseID(1) || row.Input != 16 || row.Output != 32 || row.Error != "" {
				t.Errorf("completed WebSocket Usage: response %q, tokens %d/%d, error %q", row.ResponseID, row.Input, row.Output, row.Error)
			}
		case http.StatusConflict:
			canceled++
			if row.ErrType != "extension_operation_terminal" || row.Error == "" ||
				row.ResponseID == m09ResponseID(1) || row.Input != 0 || row.Output != 0 || row.CacheRead != 0 || row.CacheWrite != 0 ||
				row.CacheWrite1h != 0 || row.Reasoning != 0 || row.Credits != nil {
				t.Errorf("interrupted WebSocket Usage recorded completion or billing: %+v", row)
			}
		default:
			t.Errorf("unexpected WebSocket Usage status %d", row.Status)
		}
	})
	if completed != 1 || canceled > 1 {
		t.Fatalf("WebSocket Usage rows completed=%d interrupted=%d, want one completed and at most one interrupted", completed, canceled)
	}
	return canceled
}
