package extension

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLinkedAgentPlanRollsBackPriorWriteOnFinalUnlinkConflict(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	catalog := filepath.Join(dir, "catalog.json")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(path, want string) {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("%s = %q, %v; want %q", path, body, err, want)
		}
	}
	write(settings, "before")
	write(catalog, "catalog")
	err := ApplyLinkedAgentPlan(struct{}{}, func(_ struct{}, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
		file, generated := snapshot.Files[0], snapshot.Files[1]
		return sdk.AgentPlan{Files: []sdk.FileChange{
			{Path: file.Path, Body: []byte("updated"), BeforeHash: file.BeforeHash},
			{Path: generated.Path, Remove: true, BeforeHash: generated.BeforeHash},
		}}, nil
	}, LinkedActionFile{Name: "settings.json", Path: settings}, LinkedActionFile{
		Name: "catalog.json", Path: catalog, UnlinkLast: true,
		BeforeWrite: func(path string) error {
			check(settings, "updated")
			write(path, "changed")
			return nil
		},
	})
	if !errors.Is(err, ErrActionChanged) {
		t.Fatalf("final unlink accepted a changed file: %v", err)
	}
	check(settings, "before")
	check(catalog, "changed")
}

func TestLinkedAgentPlanRefusesEarlyUnlink(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	catalog := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catalog, []byte("catalog"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ApplyLinkedAgentPlan(struct{}{}, func(_ struct{}, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
		file, generated := snapshot.Files[0], snapshot.Files[1]
		return sdk.AgentPlan{Files: []sdk.FileChange{
			{Path: generated.Path, Remove: true, BeforeHash: generated.BeforeHash},
			{Path: file.Path, Body: []byte("updated"), BeforeHash: file.BeforeHash},
		}}, nil
	}, LinkedActionFile{Name: "settings.json", Path: settings},
		LinkedActionFile{Name: "catalog.json", Path: catalog, UnlinkLast: true})
	if err == nil {
		t.Fatal("accepted an unlink before another planned change")
	}
	if _, err := os.Stat(settings); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published settings after refusing the plan: %v", err)
	}
	if body, err := os.ReadFile(catalog); err != nil || string(body) != "catalog" {
		t.Fatalf("catalog changed while refusing the plan: %q, %v", body, err)
	}
}
