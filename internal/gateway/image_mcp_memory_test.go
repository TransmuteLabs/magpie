package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
)

// Images the image MCP server draws are kept for an hour so a link can
// download them. Kept in memory, five 12 MB drawings were 60 MB of the
// process's heap until an hour later or the next drawing (tnt12231 on X:
// magpie's memory at 1 GB+ while drawing). They are kept on disk, and the
// reply they came in is let go.
func TestImageMCPKeepsDrawingsOffTheHeap(t *testing.T) {
	if os.Getenv("MAGPIE_IMAGE_MCP_HEAP_CHILD") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(testenv.DeadlineContext(t, time.Second), 2*time.Minute)
		defer cancel()
		command := proc.CommandContext(ctx, binary,
			"-test.run=^TestImageMCPKeepsDrawingsOffTheHeap$", "-test.count=1", "-test.timeout=2m")
		command.Env = append(os.Environ(), "MAGPIE_IMAGE_MCP_HEAP_CHILD=1")
		command.WaitDelay = time.Second
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated image heap check: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "image MCP heap child entered\n") {
			t.Fatalf("isolated image heap check did not run:\n%s", output)
		}
		return
	}
	fmt.Fprintln(os.Stdout, "image MCP heap child entered")
	s, _ := easeled(t)
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	heap := func() uint64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	body := func() []byte {
		img := make([]byte, 12<<20)
		rand.Read(img)
		copy(img, "\x89PNG\r\n\x1a\n")
		b64 := base64.StdEncoding.EncodeToString(img)
		type item struct {
			B64 string `json:"b64_json"`
		}
		b, _ := json.Marshal(map[string]any{"model": "m", "data": []item{{b64}, {b64}}})
		return b
	}()

	var urls []string
	for range 3 {
		rec := &replyRecorder{header: http.Header{}}
		rec.body.Write(body)
		res := s.imageMCPDrawn(rec, "", "http://127.0.0.1:3425")
		if res["isError"] == true {
			t.Fatalf("%v", res)
		}
		urls = append(urls, "x")
	}
	after := heap() - uint64(len(body))
	runtime.KeepAlive(body)
	st := &s.imageStore
	st.mu.Lock()
	n, kept := len(st.files), st.bytes
	st.mu.Unlock()
	if n != 6 || kept < 6*(12<<20) {
		t.Fatalf("kept %d files, %d bytes", n, kept)
	}
	// 72 MB of drawings are kept: the heap holds none of them
	if grew := int64(after) - int64(before); grew > 16<<20 {
		t.Fatalf("the heap grew by %d MB with %d MB of drawings kept", grew>>20, kept>>20)
	}
}
