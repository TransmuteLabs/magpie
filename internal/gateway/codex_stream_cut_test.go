package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func TestCodexQuietStreamRecordsOneTimeout(t *testing.T) {
	codexSignedIn(t)
	quietFast(t)
	previous := translatedQuietLongest
	t.Cleanup(func() { translatedQuietLongest = previous })
	translatedQuietLongest = 250 * time.Millisecond
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, quietCreated+quietDelta("partial reply"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)
	base := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = base })
	if err := provider.Save(provider.Provider{ID: "codex", Models: []string{"gpt-6-astra"}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	reply := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"continue"}`))
	server.Handler().ServeHTTP(reply, req)
	body := reply.Body.String()
	if reply.Code != http.StatusOK || requests.Load() != 1 ||
		!strings.Contains(body, "partial reply") || strings.Count(body, "event: response.failed\n") != 1 ||
		!strings.Contains(body, "sent nothing for") || strings.Contains(body, "before its last event") ||
		!strings.Contains(body[strings.Index(body, "event: response.failed"):], `"model":"gpt-6-astra"`) {
		t.Errorf("Codex did not receive one idle timeout after its partial reply: status=%d requests=%d body=%s", reply.Code, requests.Load(), body)
	}
	rows := usage.Load(time.Time{})
	if len(rows) != 1 || rows[0].Status != http.StatusGatewayTimeout || !strings.Contains(rows[0].Error, "sent nothing for") {
		t.Fatalf("Codex idle timeout and ledger disagree: %+v", rows)
	}
	routeHasItsCall(t, server, http.StatusGatewayTimeout)
}

// TestCodexStreamCutToldTheClient (#3377): the ChatGPT backend closed every
// relayed stream about two minutes into the reply, its deltas flowing to the
// end — no response.completed, no error event, no usage. Codex read a bare
// EOF, "stream disconnected before completion", and the row recorded a clean
// 200 that said nothing of why. A stream the upstream breaks mid-reply is
// told to the client still there as its failure, and the row says why.
func TestCodexStreamCutToldTheClient(t *testing.T) {
	codexSignedIn(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-astra"}}`,
			`data: {"type":"response.output_text.delta","delta":"half a report"}`))
		w.(http.Flusher).Flush()
		// the handler returns with no terminal event: the stream ends the
		// way the backend's cut streams did, mid-reply
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	if err := provider.Save(provider.Provider{ID: "codex", Models: []string{"gpt-6-astra"}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New().Handler())
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+CodexPath+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"write a long report"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	said := string(body)
	delta, failed := strings.Index(said, "half a report"), strings.Index(said, "response.failed")
	if delta < 0 {
		t.Fatalf("the reply's deltas never reached the client:\n%s", said)
	}
	if failed < 0 || failed < delta || !strings.Contains(said, "before its last event") {
		t.Fatalf("a stream cut mid-reply ended without its failure said after the deltas:\n%s", said)
	}
	seen := 0
	usage.Visit(time.Time{}, func(r usage.Record) {
		if r.Endpoint != CodexPath+"/responses" {
			return
		}
		seen++
		if r.Status != http.StatusBadGateway || !strings.Contains(r.Error, "before its last event") {
			t.Fatalf("the row said %d %q, not why the reply was lost", r.Status, r.Error)
		}
	})
	if seen != 1 {
		t.Fatalf("the ledger kept %d rows for the turn", seen)
	}
}

// A stream the backend cut inside a line still has its failure read: what
// magpie appends begins after a blank line, so the half line can't take
// its first event into its data, which no client parses (#3377, as #2446
// and #2580 for a relayed stream).
func TestCodexStreamCutMidLineFailureParses(t *testing.T) {
	codexSignedIn(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-astra"}}`,
			`data: {"type":"response.output_text.delta","delta":"half a report"}`))
		io.WriteString(w, `data: {"type":"response.output_text.delta","del`)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	if err := provider.Save(provider.Provider{ID: "codex", Models: []string{"gpt-6-astra"}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New().Handler())
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+CodexPath+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"write a long report"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	// every event magpie appended parses; the half line the cut left is
	// an event of its own, which a client skips
	const half = `{"type":"response.output_text.delta","del`
	failed := false
	for _, ev := range strings.Split(string(body), "\n\n") {
		var data []string
		for _, l := range strings.Split(ev, "\n") {
			if d, ok := strings.CutPrefix(l, "data:"); ok {
				data = append(data, strings.TrimSpace(d))
			}
		}
		d := strings.Join(data, "\n")
		if d == "" || d == half {
			continue
		}
		if !json.Valid([]byte(d)) {
			t.Fatalf("an event no client parses, the half line run into what magpie appended: %q\n%s", ev, body)
		}
		failed = failed || strings.Contains(d, `"response.failed"`)
	}
	if !failed {
		t.Fatalf("no response.failed after the cut:\n%s", body)
	}
}
