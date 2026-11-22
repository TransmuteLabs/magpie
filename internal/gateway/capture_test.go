package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCaptureRequestBodyLimit(t *testing.T) {
	body := []byte(strings.Repeat("x", callBodyLimit+17))
	got, truncated := captureRequestBody(body)
	if len(got) != callBodyLimit || !truncated {
		t.Fatalf("captured %d bytes, truncated=%v", len(got), truncated)
	}
	for _, n := range []int{0, 5, callBodyLimit} {
		in := []byte(strings.Repeat("y", n))
		if got, truncated := captureRequestBody(in); got != string(in) || truncated {
			t.Fatalf("%d bytes captured as %d, truncated=%v", n, len(got), truncated)
		}
	}
	// copied once: the capture is all a request's capture allocates
	if n := allocated(func() { captureRequestBody(body) }); n > callBodyLimit*5/4 {
		t.Fatalf("capturing %d bytes allocated %d", callBodyLimit, n)
	}
	// and it is a copy: the body's memory is not the capture's
	body[0] = 'z'
	if got[0] != 'x' {
		t.Fatal("the capture reads the body's memory")
	}
}

func TestCaptureRecentRequestBodyUsesCurrentLength(t *testing.T) {
	prefix := strings.Repeat("p", callBodyLimit)
	server := Server{recent: []Call{{RequestBody: prefix, RequestTruncated: true}}}
	body := []byte(prefix + "suffix")
	got, truncated := server.captureRecentRequestBody(body[:callBodyLimit])
	if got != prefix || truncated {
		t.Fatalf("exact limit: preview %d bytes, truncated %v", len(got), truncated)
	}
	server.recent[0].RequestTruncated = false
	got, truncated = server.captureRecentRequestBody(body)
	if got != prefix || !truncated {
		t.Fatalf("over limit: preview %d bytes, truncated %v", len(got), truncated)
	}
	body[0] = 'x'
	if got[0] != 'p' {
		t.Fatal("a preview hit borrowed the current body")
	}
	got, truncated = server.captureRecentRequestBody(body)
	body[0] = 'y'
	if got[0] != 'x' || !truncated {
		t.Fatal("a preview miss borrowed the current body")
	}
}

func BenchmarkCaptureRecentRequestBody(b *testing.B) {
	prefix := strings.Repeat("p", callBodyLimit)
	for _, mode := range []string{"hit", "early-edit", "late-edit", "small"} {
		b.Run(mode, func(b *testing.B) {
			server := Server{recent: []Call{{RequestBody: prefix, RequestTruncated: true}}}
			body := []byte(prefix + "new suffix")
			switch mode {
			case "early-edit":
				body[0] = 'q'
			case "late-edit":
				body[callBodyLimit-1] = 'q'
			case "small":
				body = body[:1024]
			}
			expected, expectedTruncated := captureRequestBody(body)
			b.ReportAllocs()
			var preview string
			var truncated bool
			for b.Loop() {
				preview, truncated = server.captureRecentRequestBody(body)
			}
			if preview != expected || truncated != expectedTruncated {
				b.Fatal("incorrect benchmark preview")
			}
		})
	}
}

func TestCaptureResponseWriterPreservesResponse(t *testing.T) {
	r := httptest.NewRecorder()
	w := &captureResponseWriter{ResponseWriter: r}
	_, _ = w.Write([]byte(`{"ok":true}`))
	if got := w.body.text(); got != `{"ok":true}` {
		t.Fatalf("capture = %q", got)
	}
	if got := r.Body.String(); got != `{"ok":true}` {
		t.Fatalf("response = %q", got)
	}
}
