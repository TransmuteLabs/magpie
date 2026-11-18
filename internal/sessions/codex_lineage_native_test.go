package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCodexLineageInstalledOMPReplay(test *testing.T) {
	distribution := os.Getenv("MAGPIE_OMP_TEST_DIST")
	if distribution == "" {
		test.Skip("requires an explicitly built OMP native distribution")
	}
	for _, layout := range []string{"plain", "packed", "official-schema-pair", "changed-before-apply", "changed-packed-before-apply", "config-before-apply", "package-before-apply", "disabled-before-apply"} {
		test.Run(layout, func(test *testing.T) {
			source, _, parent, prefix := lineageFixture(test)
			childText := "Selected child suffix"
			if layout == "official-schema-pair" {
				fixtures := os.Getenv("MAGPIE_OMP_TEST_FIXTURES")
				ancestor, err := os.ReadFile(filepath.Join(fixtures, "codex-sparse-ancestor.jsonl"))
				if err != nil {
					test.Fatal(err)
				}
				selected, err := os.ReadFile(filepath.Join(fixtures, "codex-sparse-child.jsonl"))
				if err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(parent, ancestor, 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(source.Path, selected, 0o600); err != nil {
					test.Fatal(err)
				}
				source.Cwd, prefix, childText = "/work/fixture", ancestor[:756], "Only the child suffix"
			}
			if layout == "packed" || layout == "changed-packed-before-apply" {
				parent = pack(test, parent, false)
				source.Path = pack(test, source.Path, false)
			}
			previous := agentThere
			agentThere = func(target string) bool { return target == "omp" }
			test.Cleanup(func() { agentThere = previous })
			ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
			defer cancel()
			manager := extension.Default()
			test.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := manager.Close(ctx); err != nil {
					test.Error(err)
				}
			})
			installed, err := manager.Install(ctx, distribution)
			if err != nil {
				test.Fatal(err)
			}
			if installed.Manifest.ID != "omp-sessions" || installed.Manifest.Version != "0.1.0" ||
				len(installed.Manifest.Sessions) != 6 || installed.Manifest.Contract.Version.Minor != 10 ||
				!installed.Manifest.Contract.Has(sdk.SessionRelatedInputs) {
				test.Fatal("installed proof requires the frozen six-route OMP package")
			}
			routes := extensionSessionRoutes("codex")
			index := slices.IndexFunc(routes, func(route SessionExtension) bool { return route.Session.To == "omp" })
			if index < 0 {
				test.Fatal("installed Codex to OMP route is unavailable")
			}
			route := routes[index]
			store, err := resolveSessionTargetStore(route.Session)
			if err != nil {
				test.Fatal(err)
			}
			policy := extension.ActionPolicy{Root: store.root, CreateUnder: store.prefix,
				BackupDir: filepath.Join(test.TempDir(), "backups")}
			options, err := sessionConversionOptions(source, "omp")
			if err != nil {
				test.Fatal(err)
			}
			selected, err := os.ReadFile(source.Path)
			if err != nil {
				test.Fatal(err)
			}
			parentOriginal, err := os.ReadFile(parent)
			if err != nil {
				test.Fatal(err)
			}
			raw := append(lineageHeader(test, lineageChildID, 3, lineageBase(lineageRootID, 3, len(prefix))),
				lineageMessage(test, 4, "user", "Selected child suffix")...)
			_, err = extension.NewActions(manager).SessionConvert(ctx, route.Plugin, sdk.Scope{},
				sdk.SessionRequest{ID: route.Session.ID, From: "codex", To: "omp", Options: options,
					Files: []sdk.SessionDocument{{Path: "selected.jsonl", Format: "codex", Body: raw}}}, policy)
			if err == nil || !strings.Contains(err.Error(), "requires captured ancestor history") {
				test.Fatalf("the installed module did not refuse uncaptured sparse history: %v", err)
			}
			plan, snapshot, err := convertExtensionSource(ctx, manager, route.Plugin, route.Session.ID, source,
				"omp", sdk.Scope{}, options, policy)
			if err != nil {
				test.Fatal(err)
			}
			lineage := snapshot.inputs.lineage
			if lineage == nil || len(lineage.segments) != 2 || lineage.segments[0].prefixHash != sha256.Sum256(prefix) ||
				lineage.segments[1].rolloutID != lineageChildID || lineage.segments[0].end == nil ||
				*lineage.segments[0].end.Offset != uint64(len(prefix)) ||
				!bytes.Contains(lineage.segments[1].metadata["history_base"], []byte(lineageRootID)) {
				test.Fatal("original ordered lineage, range, selected metadata or digest provenance was lost")
			}
			preview := plan.Preview()
			if len(preview.Files) != 1 || !bytes.Contains(preview.Files[0].Body, []byte("Inherited request")) ||
				bytes.Contains(preview.Files[0].Body, []byte("Excluded parent")) {
				test.Fatal("installed serializer did not receive the complete selected context")
			}
			output := filepath.Join(plan.Root(), filepath.FromSlash(preview.Files[0].Path))
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				test.Fatal("preview published its materialized journal")
			}
			if strings.HasSuffix(layout, "before-apply") {
				switch layout {
				case "changed-before-apply", "changed-packed-before-apply":
					altered := bytes.Clone(parentOriginal)
					if layout == "changed-packed-before-apply" {
						altered[len(altered)-1] ^= 1
					} else {
						altered = bytes.ReplaceAll(altered, []byte("Inherited answer"), []byte("Different answer"))
					}
					if err := os.WriteFile(parent, altered, 0o600); err != nil {
						test.Fatal(err)
					}
				case "config-before-apply":
					config, err := manager.ModuleConfig(ctx, route.Plugin)
					if err != nil {
						test.Fatal(err)
					}
					if _, err := manager.CompareAndSwapModuleConfig(ctx, route.Plugin, config.Version,
						json.RawMessage(`{"changed":true}`)); err != nil {
						test.Fatal(err)
					}
				case "disabled-before-apply":
					if err := manager.SetEnabled(ctx, route.Plugin, false); err != nil {
						test.Fatal(err)
					}
				case "package-before-apply":
					replaced, err := manager.Install(ctx, distribution)
					if err != nil || replaced.Revision == installed.Revision {
						test.Fatal("fixture did not replace the selected package identity", err)
					}
				}
				if _, err := plan.Execute(ctx, preview.Revision); err == nil {
					test.Fatal("installed apply accepted a changed original ancestor")
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					test.Fatal("stale installed apply wrote output")
				}
				return
			}
			Reset()
			found, exists := Find("codex", source.ID)
			if !exists || found.Path != source.Path || found.Cwd != source.Cwd {
				test.Fatalf("ordinary discovery changed the selected child: %+v", found)
			}
			result, err := CarryOver(found, "omp")
			if err != nil {
				test.Fatal(err)
			}
			if !strings.Contains(result.Command, "omp --resume "+result.ID) {
				test.Fatal("ordinary carry omitted the native resume identity")
			}
			reused, err := CarryOver(found, "omp")
			if err != nil || reused != result {
				test.Fatal("unchanged captured lineage did not reuse its checked receipt", err)
			}
			Reset()
			converted, exists := Find("omp", result.ID)
			if !exists || converted.Cwd != source.Cwd {
				test.Fatal("ordinary OMP reader did not discover the materialized result")
			}
			transcript, err := TranscriptOf(converted)
			if err != nil {
				test.Fatal(err)
			}
			var text []string
			for _, part := range transcript.Parts {
				if part.Kind == "text" {
					text = append(text, part.Text)
				}
			}
			if strings.Join(text, "|") != "Inherited request|Inherited answer|"+childText {
				encoded, _ := json.Marshal(transcript)
				test.Fatalf("ordinary transcript lost inherited context or invented a turn: %s", encoded)
			}
			after, _ := os.ReadFile(source.Path)
			parentAfter, _ := os.ReadFile(parent)
			if !bytes.Equal(after, selected) || !bytes.Equal(parentAfter, parentOriginal) {
				test.Fatal("installed replay changed its original captured sources")
			}
		})
	}
}
