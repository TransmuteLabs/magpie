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

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/library"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLibrarySDKRuleImport(t *testing.T) {
	cliHome(t)
	home := os.Getenv("HOME")
	t.Setenv("PATH", filepath.Join(home, "bin"))
	testenv.Program(t, filepath.Join(home, "bin", "claude"), "#!/bin/sh\n")
	target := filepath.Join(home, ".claude", "rules", "team-style.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := extension.Default()
	catalog := extension.NewCatalog(manager.Namespace())
	install := func(version, body string) {
		t.Helper()
		source := t.TempDir()
		manifest := sdk.Manifest{
			Schema: 1, ID: "library-fixture", Version: version, Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 17}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
			Contributions: []sdk.ContributionDescriptor{{
				Point: sdk.ContributionLibraryRule, Version: 1,
				LibraryRule: &sdk.LibraryRuleDescriptor{ID: "style", Body: body, Paths: []string{"src/**"}},
			}},
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "worker"), []byte("cannot run"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error {
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	list := func() []extension.LibraryRule {
		t.Helper()
		output, err := runOut(t, "library", "rules", "available")
		var rules []extension.LibraryRule
		if err != nil || json.Unmarshal([]byte(output), &rules) != nil {
			t.Fatalf("rule list: %s (%v)", output, err)
		}
		return rules
	}
	selectRule := func() extension.LibraryRule {
		t.Helper()
		rules := list()
		if len(rules) != 1 || rules[0].Name != "library-fixture/style" || len(rules[0].Revision) != 64 {
			t.Fatalf("unexpected rule list: %+v", rules)
		}
		return rules[0]
	}
	importRule := func(selection extension.LibraryRule) error {
		t.Helper()
		_, err := runOut(t, "library", "rules", "import", selection.Name, "team-style", "claude", "--expected", selection.Revision)
		return err
	}
	expectAbsent := func() {
		t.Helper()
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("unexpected rule file: %v", err)
		}
	}

	install("1.0.0", "Original style")
	selection := selectRule()
	expectAbsent()
	if _, err := runOut(t, "plugin", "off", "library-fixture"); err != nil {
		t.Fatal(err)
	}
	if rules := list(); len(rules) != 0 {
		t.Fatalf("disabled module offered rules: %v", rules)
	}
	if _, err := runOut(t, "plugin", "on", "library-fixture"); err != nil {
		t.Fatal(err)
	}
	if err := importRule(selection); err == nil {
		t.Fatal("selection remained valid after disable and enable")
	}
	selection = selectRule()
	install("2.0.0", "Revised style")
	if err := importRule(selection); err == nil {
		t.Fatal("selection remained valid after replacement")
	}
	expectAbsent()
	selection = selectRule()
	for _, agents := range []string{" ", ","} {
		if _, err := runOut(t, "library", "rules", "import", selection.Name, "unassigned", agents, "--expected", selection.Revision); err == nil {
			t.Fatalf("rule accepted missing agent selection %q", agents)
		}
	}
	if _, err := runOut(t, "library", "rules", "import", selection.Name, "unassigned", "none", "--expected", selection.Revision); err != nil {
		t.Fatal(err)
	}
	if err := importRule(selection); err != nil {
		t.Fatal(err)
	}
	if err := importRule(selection); err == nil || !strings.Contains(err.Error(), "already has a rule") {
		t.Fatalf("duplicate import: %v", err)
	}
	if _, err := runOut(t, "plugin", "rm", "library-fixture"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || !strings.Contains(string(data), "paths:") ||
		!strings.Contains(string(data), "src/**") || !strings.HasSuffix(string(data), "\nRevised style\n") {
		t.Fatalf("agent's saved rule: %q (%v)", data, err)
	}
	if _, err := library.RemoveRules([]string{"team-style", "unassigned"}); err != nil {
		t.Fatal(err)
	}
	expectAbsent()
}
