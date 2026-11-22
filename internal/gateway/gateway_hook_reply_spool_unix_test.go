//go:build darwin || linux

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"golang.org/x/sys/unix"
)

func TestGatewayHookSpoolReadsOnlyCompleteHTTPReplies(t *testing.T) {
	const reply = `{"id":"r1","status":"completed","output":[]}`
	const unreadOffset = 1
	request := []byte(`{"model":"fixture","stream":true}`)
	for _, fixture := range []struct {
		name, observed string
		status         int
		cut            bool
		offset         int64
	}{
		{name: "complete", status: http.StatusOK, observed: reply, offset: int64(len(reply))},
		{name: "HTTP error", status: http.StatusBadGateway, offset: unreadOffset},
		{name: "cut", status: http.StatusOK, cut: true, offset: unreadOffset},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil)
			client := httptest.NewRecorder()
			run := &p19HookReplyRun{}
			writer, finish := gatewayResponse(client, req, provider.Responses, request, run)
			capture := writer.(*conversationWriter).full
			t.Cleanup(capture.discard)
			if fixture.cut {
				capture.limit = int64(len(reply) - 1)
			}
			writer.WriteHeader(fixture.status)
			if count, err := io.WriteString(writer, reply); err != nil || count != len(reply) {
				t.Fatalf("write = (%d, %v), want %d bytes", count, err, len(reply))
			}
			if capture.f == nil {
				t.Fatal("missing spool file")
			}
			path := capture.f.Name()
			descriptor, err := unix.FcntlInt(capture.f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			probe := os.NewFile(uintptr(descriptor), path)
			defer probe.Close()
			if _, err := probe.Seek(unreadOffset, io.SeekStart); err != nil {
				t.Fatal(err)
			}

			finish()
			if offset, err := probe.Seek(0, io.SeekCurrent); err != nil || offset != fixture.offset {
				t.Errorf("spool offset = (%d, %v), want %d", offset, err, fixture.offset)
			}
			if _, err := os.Stat(path); capture.f != nil || !os.IsNotExist(err) {
				t.Errorf("spool remained after finish: file = %v, stat = %v", capture.f, err)
			}
			if !client.Flushed || client.Code != fixture.status || client.Body.String() != reply {
				t.Errorf("client = %d %q, flushed %t", client.Code, client.Body.String(), client.Flushed)
			}
			if run.calls != 1 || run.success != (fixture.observed != "") || string(run.reply) != fixture.observed || string(run.request) != string(request) {
				t.Errorf("hook = %+v, want one completion with reply %q", run, fixture.observed)
			}
		})
	}
}
