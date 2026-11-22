package extension

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogReadFileUsesBoundedSizeHint(test *testing.T) {
	dir := test.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		test.Fatal(err)
	}
	defer root.Close()
	measure := func(name string, size int) float64 {
		test.Helper()
		want := bytes.Repeat([]byte("x"), size)
		catalogTestWrite(test, filepath.Join(dir, name), want, 0o600)
		return testing.AllocsPerRun(10, func() {
			got, err := catalogReadFile(root, name, int64(size))
			if err != nil || !bytes.Equal(got, want) {
				test.Fatalf("read %q returned %d bytes, want %d: %v", name, len(got), size, err)
			}
		})
	}
	small := measure("small", 32)
	medium := measure("chunk", 12<<10)
	if medium > small+2 {
		test.Fatalf("12 KiB read allocated %.0f times, 32 B read %.0f times", medium, small)
	}
}
