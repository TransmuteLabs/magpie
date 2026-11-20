package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestClaudeWarmResumeCancellation(t *testing.T) {
	for _, stage := range []string{"previous-rewind", "write-next-turn"} {
		t.Run(stage, func(t *testing.T) {
			h := newScriptHarness(t, "unused", `
echo '{"type":"system","subtype":"init","session_id":"'$sid'"}'
while read -r line; do
  case "$line" in *'"type":"control_request"'*)
    id=$(printf '%s' "$line" | sed 's/.*"request_id":"\([^"]*\)".*/\1/')
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'"}}'
    continue ;;
  esac
  if [ -n "$file" ]; then printf '%s\n' "$line" >> "$file"; fi
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":""}'
  dd bs=1 count=1 >/dev/null 2>/dev/null || exit 17
  echo 'reading second turn' >> "$log"
  exec sleep 30
done
exit 0
`)
			h.ask("rules", "first")
			var original *subscriptionRun
			bridge := h.s.subscription
			eventually(t, "the first turn did not park its Claude worker", func() bool {
				bridge.mu.Lock()
				defer bridge.mu.Unlock()
				for _, run := range bridge.idle {
					original = run
				}
				return original != nil
			})
			var rewind chan struct{}
			if stage == "previous-rewind" {
				rewind = make(chan struct{})
				original.mu.Lock()
				original.rewinding = rewind
				original.mu.Unlock()
			}

			ctx, cancel := context.WithCancel(t.Context())
			body := `{"model":"claude-sonnet-5","max_tokens":100,"system":"rules","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]}`
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(body))
			done := make(chan struct{})
			var reason string
			t.Cleanup(func() {
				cancel()
				if rewind != nil {
					close(rewind)
				}
				select {
				case <-original.abort():
				case <-time.After(6 * time.Second):
					t.Error("the stand-in did not stop during cleanup")
				}
				select {
				case <-done:
				case <-time.After(6 * time.Second):
					t.Error("the request did not return during cleanup")
				}
			})
			go func() {
				defer close(done)
				var usage Usage
				_, reason = h.s.serveClaudeSubscription(httptest.NewRecorder(), request, provider.Anthropic, h.p, "claude-sonnet-5", []byte(body), &usage)
			}()
			if rewind != nil {
				eventually(t, "the next request did not select the warm worker", func() bool {
					bridge.mu.Lock()
					defer bridge.mu.Unlock()
					return original.idleKey == ""
				})
			} else {
				eventually(t, "the warm worker did not start reading the second turn", func() bool {
					for _, log := range h.runs() {
						if strings.Contains(log, "reading second turn") {
							return true
						}
					}
					return false
				})
			}

			cancel()
			select {
			case <-done:
				if reason != context.Canceled.Error() {
					t.Fatalf("the canceled warm request returned %q", reason)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the warm request stayed blocked after cancellation")
			}
			select {
			case <-original.exited:
			case <-time.After(6 * time.Second):
				t.Fatal("the canceled warm request left its worker running")
			}
			if runs := h.runs(); len(runs) != 1 {
				t.Fatalf("the canceled warm request launched another worker: %d logs", len(runs))
			}
		})
	}
}
