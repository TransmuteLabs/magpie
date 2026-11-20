package extension

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLinkedAgentPlanReportsTheRejectedFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "actual.toml")
	catalog := filepath.Join(dir, "actual.json")
	rejected := errors.New("invalid catalog")
	plan := func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		if settings.Files[0].Path != "config.toml" || settings.Files[1].Path != "catalog.json" {
			t.Fatalf("plan received filesystem paths: %+v", settings.Files)
		}
		return sdk.AgentPlan{}, edit.AtFile(settings.Files[1].Path, rejected)
	}
	err := ApplyLinkedAgentPlan(struct{}{}, plan,
		LinkedActionFile{Name: "config.toml", Path: config},
		LinkedActionFile{Name: "catalog.json", Path: catalog})
	if !errors.Is(err, rejected) || err.Error() != catalog+": "+rejected.Error() {
		t.Fatalf("expected original cause and physical catalog path, got %v", err)
	}
}

func TestLinkedAgentPlanRejectsChangedInputBeforePublishing(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "edited"
		if replace {
			name = "replaced with identical bytes"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "settings.json")
			catalog := filepath.Join(dir, "models.json")
			put := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			put(config, "original")
			plan := func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
				if replace {
					replacement := filepath.Join(dir, "replacement")
					put(replacement, "original")
					if err := os.Rename(replacement, config); err != nil {
						t.Fatal(err)
					}
				} else {
					put(config, "user edit")
				}
				result := sdk.AgentPlan{}
				for _, file := range settings.Files {
					result.Files = append(result.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: []byte("planned")})
				}
				return result, nil
			}
			err := ApplyLinkedAgentPlan(struct{}{}, plan,
				LinkedActionFile{Name: "models.json", Path: catalog},
				LinkedActionFile{Name: "settings.json", Path: config})
			if !errors.Is(err, ErrActionChanged) {
				t.Fatalf("accepted changed input: %v", err)
			}
			if _, err := os.Stat(catalog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published catalog before rejecting plan: %v", err)
			}
			want := "user edit"
			if replace {
				want = "original"
			}
			if got, err := os.ReadFile(config); err != nil || string(got) != want {
				t.Fatalf("plan overwrote original input: %q, %v", got, err)
			}
		})
	}
}

func TestLinkedAgentPlanRollsBackOnlyItsOwnPublication(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new file"
		if existing {
			name = "existing file"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "settings.json")
			catalog := filepath.Join(dir, "models.json")
			put := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			put(config, "initial config")
			if existing {
				put(catalog, "initial catalog")
			}
			plan := func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
				result := sdk.AgentPlan{}
				for _, file := range settings.Files {
					result.Files = append(result.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: []byte("planned")})
				}
				return result, nil
			}
			refusal := errors.New("config publication refused")
			err := edit.Atomically(func() error {
				put(config, "user config")
				if existing {
					put(catalog, "user catalog")
				}
				return ApplyLinkedAgentPlan(struct{}{}, plan,
					LinkedActionFile{Name: "models.json", Path: catalog},
					LinkedActionFile{Name: "settings.json", Path: config, BeforeWrite: func(string) error {
						if got, err := os.ReadFile(catalog); err != nil || string(got) != "planned" {
							t.Fatalf("catalog was not published before refusal: %q, %v", got, err)
						}
						return refusal
					}})
			}, config, catalog)
			if !errors.Is(err, refusal) {
				t.Fatalf("publication error: %v", err)
			}
			if got, err := os.ReadFile(config); err != nil || string(got) != "user config" {
				t.Errorf("user config was lost: %q, %v", got, err)
			}
			if got, err := os.ReadFile(catalog); existing && (err != nil || string(got) != "user catalog") ||
				!existing && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("catalog rollback restored the wrong version: %q, %v", got, err)
			}
		})
	}
}
