package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/settings"
)

func BenchmarkNativeHistoryAuthority(b *testing.B) {
	for _, count := range []int{1, 96} {
		b.Run(fmt.Sprintf("consents=%d", count), func(b *testing.B) {
			dir := b.TempDir()
			b.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			if err := os.MkdirAll(appdir.Config(), 0o700); err != nil {
				b.Fatal(err)
			}
			write := func(path string, value any) {
				b.Helper()
				body, err := json.Marshal(value)
				if err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					b.Fatal(err)
				}
			}
			write(settings.Path(), map[string]any{
				"sessionReads": map[string]any{"asked": true, "agents": map[string]bool{"claude": true}},
			})
			digest := strings.Repeat("a", 64)
			write(sourceBindingsPath(), []SessionSourceBinding{{
				Plugin: "fixture", Source: "codex", Agent: "codex", Identity: digest,
			}})
			consents := make([]StorageSourceConsent, count)
			for index := range consents {
				consents[index] = StorageSourceConsent{
					Plugin: fmt.Sprintf("fixture-%d", index), Source: "sessions",
					Agent: "other", RootSlot: "home", RootRef: digest, Identity: digest,
				}
			}
			write(storageConsentsPath(), consents)
			path := filepath.Join(dir, "fixture.jsonl")
			body := `{"type":"user","message":{"role":"user","content":"snapshot needle"}}` + "\n" +
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fixture reply"}]}}` + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				b.Fatal(err)
			}
			session := Session{Agent: "claude", ID: "fixture", Path: path, Resume: "claude --resume fixture"}
			b.Run("Transcript", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					result, err := TranscriptOfContext(b.Context(), session)
					if err != nil || len(result.Parts) != 2 || result.Parts[0].Text != "snapshot needle" {
						b.Fatalf("transcript: %+v, %v", result, err)
					}
				}
			})
			b.Run("TranscriptAt", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					result, err := TranscriptAtContext(b.Context(), session, "needle")
					if err != nil || len(result.Parts) != 2 || result.Parts[1].Text != "fixture reply" {
						b.Fatalf("transcript: %+v, %v", result, err)
					}
				}
			})
			b.Run("Resume", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := ResumeNowContext(b.Context(), session); got != session.Resume {
						b.Fatalf("resume: %q", got)
					}
				}
			})
		})
	}
}
