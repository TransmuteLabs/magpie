package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/library"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLibrarySDKContributionPublication(t *testing.T) {
	for _, fixture := range []struct {
		kind         string
		contribution sdk.ContributionDescriptor
	}{
		{"rules", sdk.ContributionDescriptor{
			Point: sdk.ContributionLibraryRule, Version: 1,
			LibraryRule: &sdk.LibraryRuleDescriptor{ID: "review", Body: "Read the diff."},
		}},
		{"commands", sdk.ContributionDescriptor{
			Point: sdk.ContributionLibraryCommand, Version: 1,
			LibraryCommand: &sdk.LibraryCommandDescriptor{ID: "review", Prompt: "Read the diff."},
		}},
	} {
		t.Run(fixture.kind, func(t *testing.T) {
			cliHome(t)
			home := os.Getenv("HOME")
			t.Setenv("PATH", filepath.Join(home, "bin"))
			testenv.Program(t, filepath.Join(home, "bin", "claude"), "#!/bin/sh\n")
			claude := filepath.Join(home, ".claude")
			if err := os.MkdirAll(claude, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := library.SaveCommand("", library.Command{Name: "existing", Prompt: "Stay as saved."}); err != nil {
				t.Fatal(err)
			}
			config, err := edit.Target(filepath.Join(settings.Dir(), "library.json"))
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(claude, fixture.kind, "review.md")
			const module = "library-publication"
			source := t.TempDir()
			manifest := sdk.Manifest{
				Schema: 1, ID: module, Version: "1.0.0", Runtime: "exec", Entry: "worker",
				Contract: sdk.Contract{
					Version: sdk.APIVersion{Major: 1, Minor: 18}, Capabilities: []sdk.Capability{sdk.ModuleRPC},
				},
				Contributions: []sdk.ContributionDescriptor{fixture.contribution},
			}
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "worker"), []byte("cannot run"), 0o700); err != nil {
				t.Fatal(err)
			}
			catalog := extension.NewCatalog(extension.Default().Namespace())
			if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error {
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			importWhileDisabling := func(path string) error {
				t.Helper()
				output, err := runOut(t, "library", fixture.kind, "available")
				var choices []struct {
					Name     string `json:"name"`
					Revision string `json:"revision"`
				}
				if err != nil || json.Unmarshal([]byte(output), &choices) != nil || len(choices) != 1 {
					t.Fatalf("active contribution: %s (%v)", output, err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				started := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					select {
					case <-started:
						done <- catalog.SetEnabled(ctx, module, false)
					case <-ctx.Done():
						done <- ctx.Err()
					}
				}()
				var visited bool
				var disableErr error
				importErr := edit.Filtered(func(written string, data []byte) []byte {
					if filepath.Clean(written) != filepath.Clean(path) || visited {
						return data
					}
					visited = true
					close(started)
					disableErr = <-done
					return data
				}, func() error {
					_, err := runOut(t, "library", fixture.kind, "import", choices[0].Name, "review",
						"claude", "--expected", choices[0].Revision)
					return err
				})
				if !visited {
					cancel()
					<-done
					t.Fatalf("the import did not reach %s: %v", path, importErr)
				}
				if disableErr != nil {
					t.Fatalf("disable while writing %s: %v", path, disableErr)
				}
				return importErr
			}

			if err := importWhileDisabling(config); err == nil || !strings.Contains(err.Error(), "revoked") {
				t.Fatalf("import published after disable: %v", err)
			}
			if data, err := os.ReadFile(config); err != nil || string(data) != string(before) {
				t.Fatalf("revoked import changed Library: %q (%v)", data, err)
			}
			if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("revoked import created the agent's file: %v", err)
			}
			if err := catalog.SetEnabled(t.Context(), module, true); err != nil {
				t.Fatal(err)
			}
			targetPath, err := edit.Target(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := importWhileDisabling(targetPath); err != nil {
				t.Fatalf("published copy could not sync after disable: %v", err)
			}
			if data, err := os.ReadFile(target); err != nil || !strings.Contains(string(data), "Read the diff.") {
				t.Fatalf("saved copy missing from the agent: %q (%v)", data, err)
			}
		})
	}
}
