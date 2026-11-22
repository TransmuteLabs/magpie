package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
)

const codexProbePrompt = "Say only fixture-complete. M02-original."
const codexProbeQueued = "M02-queued"
const codexProbePartial = "fixture-partial"

type codexProbeCounts struct {
	mu sync.Mutex

	sockets, closes1013, httpPosts, wsCreates, warmups int
	wsPrevious, httpPrevious, upstreamPrevious         int
	upstream, queued, fullInput, completions           int
	injected                                           bool
}

func TestStockCodexQueueOverflowRecovery(t *testing.T) {
	binary := os.Getenv("MAGPIE_TEST_STOCK_CODEX")
	if binary == "" {
		t.Skip("set MAGPIE_TEST_STOCK_CODEX to the installed Codex binary")
	}
	if runtime.GOOS != "darwin" || !filepath.IsAbs(binary) {
		t.Fatal("requires macOS Seatbelt and an absolute Codex executable path")
	}
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	var counts codexProbeCounts
	stopped := make(chan struct{})
	vendorKey := rand.Text()
	probeMarker := rand.Text()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+vendorKey {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		counts.mu.Lock()
		counts.upstream++
		attempt := counts.upstream
		if bytes.Contains(body, []byte(codexProbeQueued)) {
			counts.queued++
		}
		if bytes.Contains(body, []byte("M02-original")) {
			counts.fullInput++
		}
		if gjson.GetBytes(body, "previous_response_id").Exists() {
			counts.upstreamPrevious++
		}
		counts.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("resp_m02_%d", attempt)
		reply := `{"id":"` + id + `","object":"response","status":"in_progress","output":[]}`
		_, _ = io.WriteString(w, sse(
			`data: {"type":"response.created","sequence_number":0,"response":`+reply+`}`,
			`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
			`data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
			`data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"`+codexProbePartial+`","logprobs":[]}`))
		w.(http.Flusher).Flush()
		if attempt == 1 {
			<-r.Context().Done()
			close(stopped)
			return
		}
		item := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + codexProbePartial + `","annotations":[]}]}`
		_, _ = io.WriteString(w, sse(
			`data: {"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":`+item+`}`,
			`data: {"type":"response.completed","sequence_number":5,"response":{"id":"`+id+`","object":"response","status":"completed","output":[`+item+`],"usage":{"input_tokens":12,"output_tokens":1,"total_tokens":13}}}`))
		counts.mu.Lock()
		counts.completions++
		counts.mu.Unlock()
	}))
	t.Cleanup(upstream.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fixture", Key: vendorKey, Responses: upstream.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	_, magpie := sockServer(t)
	target, err := url.Parse(magpie.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	var connections sync.WaitGroup
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		defer connections.Done()
		if r.Header.Get("X-Magpie-Probe") != probeMarker {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		r.Header.Del("X-Magpie-Probe")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/responses" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
			recordCodexOverflow(ctx, w, r, "ws"+strings.TrimPrefix(magpie.URL, "http")+r.URL.Path, &counts)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			counts.mu.Lock()
			counts.httpPosts++
			if gjson.GetBytes(body, "previous_response_id").Exists() {
				counts.httpPrevious++
			}
			counts.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			proxy.ServeHTTP(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			proxy.ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		cancel()
		recorder.Close()
		connections.Wait()
	})

	exit, runErr := runStockCodexProbe(t, ctx, binary, recorder.URL, probeMarker)
	counts.mu.Lock()
	defer counts.mu.Unlock()
	t.Logf("exit=%d ws=%d genuine_1013=%d injected=%t warmups=%d ws_creates=%d ws_previous=%d http_posts=%d http_previous=%d upstream=%d upstream_previous=%d queued_upstream=%d full_input_upstream=%d completions=%d",
		exit, counts.sockets, counts.closes1013, counts.injected, counts.warmups, counts.wsCreates, counts.wsPrevious,
		counts.httpPosts, counts.httpPrevious, counts.upstream, counts.upstreamPrevious, counts.queued, counts.fullInput, counts.completions)
	if ctx.Err() != nil || runErr != nil {
		t.Fatalf("Codex did not finish within the bounded probe: exit=%d setup_error=%t timeout=%t", exit, runErr != nil, ctx.Err() != nil)
	}
	if !counts.injected || counts.closes1013 != 1 || counts.queued != 0 || counts.upstreamPrevious != 0 || counts.fullInput != counts.upstream {
		t.Fatal("missing genuine overflow or unexpected upstream delivery")
	}
	select {
	case <-stopped:
		t.Log("first upstream request canceled")
	case <-ctx.Done():
		t.Fatal("first upstream request did not cancel")
	}
}

func recordCodexOverflow(parent context.Context, w http.ResponseWriter, r *http.Request, target string, counts *codexProbeCounts) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	codex, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer codex.CloseNow()
	headers := make(http.Header)
	for _, name := range []string{"User-Agent", "Originator", "OpenAI-Beta", "X-Codex-Turn-State", "X-Codex-Turn-Metadata"} {
		if value := r.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	magpie, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		return
	}
	defer magpie.CloseNow()
	codex.SetReadLimit(2 << 20)
	magpie.SetReadLimit(2 << 20)
	counts.mu.Lock()
	counts.sockets++
	counts.mu.Unlock()
	var write sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			kind, body, err := codex.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			frame := gjson.ParseBytes(body)
			counts.mu.Lock()
			if frame.Get("type").String() == "response.create" {
				if generate := frame.Get("generate"); generate.Exists() && !generate.Bool() {
					counts.warmups++
				} else {
					counts.wsCreates++
				}
				if frame.Get("previous_response_id").Exists() {
					counts.wsPrevious++
				}
			}
			counts.mu.Unlock()
			write.Lock()
			err = magpie.Write(ctx, kind, body)
			write.Unlock()
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		<-done
	}()
	for {
		kind, body, err := magpie.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status == websocket.StatusTryAgainLater {
				counts.mu.Lock()
				counts.closes1013++
				counts.mu.Unlock()
				var closed websocket.CloseError
				errors.As(err, &closed)
				_ = codex.Close(status, closed.Reason)
			}
			return
		}
		if err := codex.Write(ctx, kind, body); err != nil {
			return
		}
		if gjson.GetBytes(body, "delta").String() != codexProbePartial {
			continue
		}
		counts.mu.Lock()
		inject := !counts.injected
		counts.injected = true
		counts.mu.Unlock()
		if inject {
			write.Lock()
			for index := range 9 {
				item := fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"%s-%d"}]}`, codexProbeQueued, index)
				if err := magpie.Write(ctx, websocket.MessageText, []byte(create("", item))); err != nil {
					break
				}
			}
			write.Unlock()
		}
	}
}

func runStockCodexProbe(t *testing.T, ctx context.Context, binary, address, marker string) (int, error) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	account, err := osuser.Current()
	if err != nil || account.HomeDir == "" {
		t.Fatal("cannot isolate macOS account home")
	}
	for _, dir := range []string{".codex", "work", "tmp", "xdg"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := fmt.Sprintf(`model = "fake/m1"
model_provider = "fixture"
cli_auth_credentials_store = "file"
check_for_update_on_startup = false
web_search = "disabled"

[analytics]
enabled = false

[feedback]
enabled = false

[model_providers.fixture]
name = "Loopback fixture"
base_url = %q
wire_api = "responses"
supports_websockets = true
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 2
http_headers = {"X-Magpie-Probe" = %q}
`, address+"/v1", marker)
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	profile := fmt.Sprintf(`(version 1)
(allow default)
(deny network*)
(allow network-outbound (remote ip "localhost:%s"))
(deny file-read* (subpath %q))
(deny file-write*)
(allow file-write* (subpath %q) (literal "/dev/null"))
`, port, account.HomeDir, home)
	cmd := proc.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, binary,
		"exec", "--skip-git-repo-check", "--ephemeral", "--json", codexProbePrompt)
	cmd.Dir = filepath.Join(home, "work")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"), "TMPDIR=" + filepath.Join(home, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"), "XDG_CACHE_HOME=" + filepath.Join(home, "xdg"),
		"XDG_DATA_HOME=" + filepath.Join(home, "xdg")}
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		diagnostics := strings.ToLower(stderr.String())
		t.Logf("codex diagnostics: seatbelt_error=%t config_error=%t operation_denied=%t",
			strings.Contains(diagnostics, "sandbox-exec:"), strings.Contains(diagnostics, "error loading config"),
			strings.Contains(diagnostics, "operation not permitted"))
		return cmd.ProcessState.ExitCode(), nil
	}
	return -1, err
}
