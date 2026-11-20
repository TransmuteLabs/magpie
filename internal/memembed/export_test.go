package memembed

import (
	"os"
	"path/filepath"
	"testing"
)

var (
	IndexForTest    = index
	CacheDirForTest = dir
)

func VectorCountForTest(path string) int { return len(readVecs(path)) }

type testEmbedder func(string) []float32

func (embed testEmbedder) Embed(text string) []float32 { return embed(text) }

func UseEmbedderForTest(embed func(string) []float32) {
	loadModel = func(string) (embedder, error) { return testEmbedder(embed), nil }
}

func ModelHomeForTest(t *testing.T, path string) {
	t.Helper()
	previousRoot, previousState, previousLoad := Root, StatePath, loadModel
	Root = func() string { return filepath.Join(path, "model") }
	StatePath = func() string { return filepath.Join(path, "memory-semantic.json") }
	t.Cleanup(func() {
		Root, StatePath, loadModel = previousRoot, previousState, previousLoad
		unload()
		store.mu.Lock()
		store.v = nil
		store.mu.Unlock()
	})
}

func SparseModelForTest(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, modelFile := range files {
		path := filepath.Join(dir(), filepath.Base(modelFile.path))
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, modelFile.size); err != nil {
			t.Fatal(err)
		}
	}
}
