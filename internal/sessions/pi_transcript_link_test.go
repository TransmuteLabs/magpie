package sessions

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestPiFamilyReadsViaLinkedTreeCodec(t *testing.T) {
	before := piTranscriptCodec
	t.Cleanup(func() { piTranscriptCodec = before })
	var visited []string
	piTranscriptCodec = sdk.SessionTreeCodec{
		Inspect: before.Inspect,
		Decode: func(line []byte, spokenOnly bool) (sdk.SessionTreeRecord, error) {
			record, err := before.Decode(line, spokenOnly)
			if err != nil {
				return record, err
			}
			parts := record.Parts
			record.Parts = func() []Part {
				visited = append(visited, record.ID)
				return append(parts(), Part{Role: "assistant", Kind: "text", Text: "decoded by the linked codec"})
			}
			return record, nil
		},
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, path,
		`{"type":"message","id":"question","message":{"role":"user","content":"What changed?"}}`,
		`{"type":"message","id":"obsolete","parentId":"question","message":{"role":"assistant","content":"Earlier answer"}}`,
		`{"type":"message","id":"answer","parentId":"question","message":{"role":"assistant","model":"fixture-model","content":"Current answer"}}`,
	)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	compressed := path + ".zst"
	if err := os.WriteFile(compressed, encoder.EncodeAll(content, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"pi", "omp", "raft", "cindy"} {
		t.Run(agent, func(t *testing.T) {
			for _, file := range []string{path, compressed} {
				visited = nil
				got, err := TranscriptOfContext(context.Background(), Session{Agent: agent, ID: "fixture", Path: file})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(visited, []string{"question", "answer"}) {
					t.Fatalf("%s bypassed the linked decoder for the current branch: visited %v", file, visited)
				}
				want := []string{"user/text: What changed?", "assistant/text: decoded by the linked co",
					"assistant/text: Current answer", "assistant/text: decoded by the linked co"}
				if actual := kinds(got.Parts); !slices.Equal(actual, want) || got.Parts[2].Model != "fixture-model" {
					t.Fatalf("transcript parts: %+v", got.Parts)
				}
			}
		})
	}
}
