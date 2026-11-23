package memdistill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/memory"
	"github.com/yetone/magpie/internal/sessions"
)

const piDistillBranch = `{"type":"session","id":"fixture"}
{"type":"message","id":"first","parentId":null,"message":{"role":"user","content":"Use portable checklists."}}
{"type":"message","id":"left","parentId":"first","message":{"role":"user","content":"Delete the old notes."}}
{"type":"message","id":"answer","parentId":"left","message":{"role":"assistant","content":"Obsolete reply."}}
{"type":"compaction","id":"compact","parentId":"first","summary":"Compacted context has its own kind."}
{"type":"branch_summary","id":"selected","parentId":"compact","summary":"Branch summary is not user testimony."}
{"type":"message","id":"answer","p\u0061rentId":"selected","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden reasoning"},{"type":"text","text":"The checklist is ready."}]},"message":{"model":"test-model"}}
{"type":"message","id":"last-user","parentId":"answer","message":{"role":"user","content":[{"type":"text","text":"Keep a signed record."}]}}
{"type":"message","id":"last-assistant","parentId":"last-user","message":{"role":"assistant","content":"The record is signed."}}
{"type":"custom","id":"head","parentId":"last-assistant","data":{"id":"nested","parentId":"left","type":"message"}}
{"type":"message","id":"bad-message","parentId":"left","message":{"role":"assistant","model":99}}
{"type":"compaction","id":"bad-summary","parentId":"left","SUMMARY":false}
{"type":"message","id":"malformed","parentId":"left","message":{"role":"user","content":"stray"}
`

const piDistillTail = `{"type":"message","id":"tail","parentId":"left","message":{"role":"user","content":"This is not a committed line."}}`

const piDistillConversation = "User: Use portable checklists.\n\nAssistant: The checklist is ready.\n\nUser: Keep a signed record.\n\nAssistant: The record is signed."

func TestReadAllPiTreeKeepsPromptAndEvidence(t *testing.T) {
	for _, name := range []string{"plain", "zstd"} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t)
			read = transcriptOf
			on(t, "")
			file := filepath.Join(sb.dir, "session.jsonl")
			cwd := filepath.Join(sb.dir, "proj")
			if err := os.Mkdir(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			content := []byte(piDistillBranch + piDistillTail)
			if name == "zstd" {
				encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = encoder.Close() })
				content = encoder.EncodeAll(content, nil)
				file += ".zst"
			}
			if err := os.WriteFile(file, content, 0o600); err != nil {
				t.Fatal(err)
			}
			sb.list = []sessions.Session{{Agent: "pi", ID: "fixture", Cwd: cwd, Path: file, Last: sb.at.Add(-time.Hour), Transcript: true}}
			hits := 0
			result, err := ReadAll(t.Context(), "auto/model", func(_ context.Context, model, effort, system, user string) (string, error) {
				hits++
				_, got, found := strings.Cut(user, "Conversation:\n\n")
				if model != "auto/model" || effort != "" || system == "" || !found || got != piDistillConversation {
					t.Fatalf("model=%q effort=%q system=%q conversation=%q", model, effort, system, got)
				}
				return `[
					{"text":"Use a portable checklist","kind":"rule","scope":"project","quote":"Use portable checklists"},
					{"text":"Discard all prior notes","kind":"rule","scope":"project","quote":"Delete the old notes"},
					{"text":"Treat branch summaries as dialogue","kind":"rule","scope":"project","quote":"Branch summary is not user testimony"},
					{"text":"Read incomplete writes","kind":"rule","scope":"project","quote":"This is not a committed line"}
				]`, nil
			})
			if err != nil || result != (Result{Read: 1, Added: 1}) || hits != 1 {
				t.Fatalf("distilled %+v, completed %d, err %v", result, hits, err)
			}
			rows := memories(t)
			if len(rows) != 1 {
				t.Fatalf("saved %d memories, want 1", len(rows))
			}
			row := rows[0]
			if row.Text != "Use a portable checklist" || row.Kind != "rule" || row.Status != memory.Candidate ||
				!strings.HasPrefix(row.Scope, "project:") || row.Origin != memory.FromInferred || len(row.Sources) != 1 {
				t.Fatalf("invalid saved candidate: %+v", row)
			}
			source := row.Sources[0]
			if source.Agent != "pi" || source.Session != "fixture" || source.Dir != cwd || source.Quote != "Use portable checklists" {
				t.Fatalf("invalid evidence: %+v", source)
			}
		})
	}
}

func BenchmarkPiDistillTranscript(b *testing.B) {
	for _, fixture := range []struct{ name, payload string }{
		{"short", `unused tool output`},
		{"large", strings.Repeat(`{\"word\":\"value\"}\n`, 2048)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			var input strings.Builder
			for index := range 48 {
				fmt.Fprintf(&input, `{"type":"message","id":"old-%d","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"%s"}]}}`+"\n", index, fixture.payload)
			}
			input.WriteString(piDistillBranch)
			file := filepath.Join(b.TempDir(), "session.jsonl")
			if err := os.WriteFile(file, []byte(input.String()), 0o600); err != nil {
				b.Fatal(err)
			}
			session := sessions.Session{Agent: "pi", ID: "fixture", Path: file, Transcript: true}
			discussion, err := transcriptOf(b.Context(), session)
			text, turns := transcript(discussion)
			if err != nil || text != piDistillConversation || turns != 2 {
				b.Fatalf("invalid benchmark input: %q, %d, %v", text, turns, err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(input.Len()))
			for b.Loop() {
				if _, err := transcriptOf(b.Context(), session); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
