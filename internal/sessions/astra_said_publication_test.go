package sessions

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/extension"
)

func TestAstraSaidCollectedDelegatedAuthority(t *testing.T) {
	for _, foreignDisable := range []bool{false, true} {
		name := "unchanged-authority"
		if foreignDisable {
			name = "foreign-disable-after-first-hit"
		}
		t.Run(name, func(t *testing.T) {
			t.Log("ASTRA_SAID_DELEGATED_PUBLICATION_ENTRY")
			_, installed, sourcePath := installedSourceFixture(t)
			if err := SetReads(nil, map[string]bool{"fixture-source": true, "claude": true}); err != nil {
				t.Fatal(err)
			}
			sourceFixtureWrite(t, sourcePath, "astradelegatedprivateword", 7)
			nativePath := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-work-astra", "native-later.jsonl")
			writeLines(t, nativePath, `{"type":"user","timestamp":"2026-11-12T00:00:00Z","message":{"role":"user","content":"astradelegatedprivateword"},"cwd":"/work/astra","sessionId":"native-later","uuid":"astra-one"}`)
			warm, err := SearchSaid(t.Context(), NeedleOf("astradelegatedprivateword"), "", 10)
			if err != nil || len(warm.Hits) != 2 || warm.Hits[0].Session.Agent != "fixture-source" ||
				warm.Hits[1].Session.Agent != "claude" {
				t.Fatalf("actual installed source/native positive setup: hits=%v err=%v", saidAgents(warm), err)
			}
			t.Logf("ordinary SearchSaid positive: hits=%v installed=%s revision=%s", saidAgents(warm), installed.Manifest.ID, installed.Revision)
			sourceRecord := warm.Hits[0].Session.source
			var once sync.Once
			var reached bool
			scanHook = func(path string, _ int64) {
				if path != nativePath {
					return
				}
				once.Do(func() {
					reached = true
					epoch, view := sourceEpoch.Load(), sourceView.Load()
					t.Log("ASTRA_SECOND_READ_BARRIER ordinary native parser reached")
					if foreignDisable {
						catalog := extension.NewCatalog(filepath.Dir(extension.CatalogPath()))
						if err := catalog.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
							t.Fatal(err)
						}
						if err := checkSelectedSource(t.Context(), sourceRecord); err == nil {
							t.Fatal("foreign catalog disable did not revoke the selected record")
						}
						if epoch != sourceEpoch.Load() || view != sourceView.Load() {
							t.Fatal("foreign catalog control unexpectedly notified local source publication")
						}
						t.Log("ASTRA_FOREIGN_DISABLE durable grant rejected; local epoch/view unchanged")
					}
				})
			}
			t.Cleanup(func() { scanHook = nil })
			result, err := SearchSaid(t.Context(), NeedleOf("astradelegatedprivateword"), "", 10)
			if !reached {
				t.Fatal("second ordinary search did not reach native read barrier")
			}
			t.Logf("ordinary SearchSaid publication: hits=%v err=%v", saidAgents(result), err)
			if !foreignDisable {
				if err != nil || len(result.Hits) != 2 {
					t.Fatalf("unchanged authority lost valid hits: hits=%v err=%v", saidAgents(result), err)
				}
				return
			}
			for _, hit := range result.Hits {
				if hit.Session.Agent == "fixture-source" {
					t.Fatalf("ASTRA-SAID-PUBLICATION-01: ordinary SearchSaid published revoked delegated transcript: match=%q err=%v", hit.Hit.Match, err)
				}
			}
		})
	}
}
