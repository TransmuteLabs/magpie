package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEmbedderReleasesONNXGraph(t *testing.T) {
	dir := os.Getenv("MAGPIE_EMBED_DIR")
	if dir == "" {
		t.Skip("MAGPIE_EMBED_DIR is not set")
	}
	var before, loaded runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	net, err := loadEmbedNet(filepath.Join(dir, "model_int8.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&loaded)
	live := int64(loaded.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("network live heap: %.2f MiB", float64(live)/(1<<20))
	runtime.KeepAlive(net)
	if live > 128<<20 {
		t.Fatalf("network retains %.2f MiB after loading its 91.6 MiB word table and organized weights", float64(live)/(1<<20))
	}
}
