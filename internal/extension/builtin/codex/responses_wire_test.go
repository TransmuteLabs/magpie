package codex

import (
	"bytes"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The encoder's coders and their histories live as long as the process, so
// how many it keeps is set by magpie, not by the machine's cores: 28 turns
// of 1 MiB at once on 16 cores held ~96 MB with the library's default of a
// coder per core.
func TestCodexZstdHoldsLittle(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(16))
	var b bytes.Buffer
	for i := 0; b.Len() < 1<<20; i++ {
		b.WriteString(`{"role":"user","content":"line `)
		b.WriteString(strings.Repeat("x", i*7919%61))
		b.WriteString(`"},`)
	}
	src := b.Bytes()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	z := newResponsesZstd()
	var wg sync.WaitGroup
	for range 28 {
		wg.Add(1)
		go func() { defer wg.Done(); z.EncodeAll(src, nil) }()
	}
	wg.Wait()
	runtime.GC()
	runtime.ReadMemStats(&after)
	if held := int64(after.HeapInuse) - int64(before.HeapInuse); held > 24<<20 {
		t.Fatalf("the encoder holds %d MB after 28 turns at once", held>>20)
	}
	runtime.KeepAlive(z)
}
