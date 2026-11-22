package extension

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLibraryListingsReadOnlyRelevantEnablement(t *testing.T) {
	manager := NewManager(t.TempDir())
	ctx := t.Context()
	rule := sdk.ContributionDescriptor{
		Point: sdk.ContributionLibraryRule, Version: 1,
		LibraryRule: &sdk.LibraryRuleDescriptor{ID: "review", Body: "Read the diff."},
	}
	command := sdk.ContributionDescriptor{
		Point: sdk.ContributionLibraryCommand, Version: 1,
		LibraryCommand: &sdk.LibraryCommandDescriptor{ID: "review", Prompt: "Read the diff."},
	}
	var files []string
	for _, fixture := range []struct {
		module        string
		contributions []sdk.ContributionDescriptor
	}{
		{"rule-source", []sdk.ContributionDescriptor{rule}},
		{"command-source", []sdk.ContributionDescriptor{command}},
		{"other-module", nil},
		{"unknown-epoch", []sdk.ContributionDescriptor{rule, command}},
	} {
		source := t.TempDir()
		manifest := sdk.Manifest{
			Schema: 1, ID: fixture.module, Version: "1.0.0", Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{
				Version: sdk.APIVersion{Major: 1, Minor: 18}, Capabilities: []sdk.Capability{sdk.ModuleRPC},
			},
			Contributions: fixture.contributions,
		}
		catalogTestWriteManifest(t, source, manifest)
		catalogTestWrite(t, filepath.Join(source, manifest.Entry), []byte("metadata fixture"), 0o700)
		catalogTestInstall(t, manager.catalog, source)
		if err := manager.catalog.SetEnabled(ctx, fixture.module, false); err != nil {
			t.Fatal(err)
		}
		if err := manager.catalog.SetEnabled(ctx, fixture.module, true); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(manager.Namespace(), observerEnablementFile(fixture.module))
		if fixture.module == "unknown-epoch" {
			if err := os.WriteFile(path, []byte(`"?"`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		files = append(files, path)
	}

	t.Run("rules", func(t *testing.T) {
		got := countExtensionFileReads(t, files, func() {
			rules, err := manager.LibraryRules(ctx)
			if err != nil || len(rules) != 1 || rules[0].Name != "rule-source/review" ||
				rules[0].Body != rule.LibraryRule.Body || !catalogDigest(rules[0].Revision) {
				t.Fatalf("invalid rule listing: %+v, %v", rules, err)
			}
		})
		if want := []int{1, 0, 0, 1}; !slices.Equal(got, want) {
			t.Fatalf("rule listing opened module enablement markers %v, want %v", got, want)
		}
	})
	t.Run("commands", func(t *testing.T) {
		got := countExtensionFileReads(t, files, func() {
			commands, err := manager.LibraryCommands(ctx)
			if err != nil || len(commands) != 1 || commands[0].Name != "command-source/review" ||
				commands[0].Prompt != command.LibraryCommand.Prompt || !catalogDigest(commands[0].Revision) {
				t.Fatalf("invalid command listing: %+v, %v", commands, err)
			}
		})
		if want := []int{0, 1, 0, 1}; !slices.Equal(got, want) {
			t.Fatalf("command listing opened module enablement markers %v, want %v", got, want)
		}
	})
}
