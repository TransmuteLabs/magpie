package extension

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestLinkedAgentPlanRejectsOversizedOutputBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "settings.json")
	catalog := filepath.Join(dir, "models.json")
	if err := os.WriteFile(config, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := make([]byte, MaxActionBytes)
	planner := func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		return sdk.AgentPlan{Files: []sdk.FileChange{
			{Path: "settings.json", BeforeHash: settings.Files[0].BeforeHash, Body: []byte("updated")},
			{Path: "models.json", BeforeHash: settings.Files[1].BeforeHash, Body: oversized},
		}}, nil
	}
	err := ApplyLinkedAgentPlan(struct{}{}, planner,
		LinkedActionFile{Name: "settings.json", Path: config},
		LinkedActionFile{Name: "models.json", Path: catalog})
	if err == nil || !strings.Contains(err.Error(), "output exceeds its byte budget") {
		t.Fatalf("oversized plan was not rejected: %v", err)
	}
	if body, err := os.ReadFile(config); err != nil || string(body) != "original" {
		t.Fatalf("rejected plan changed settings: %q, %v", body, err)
	}
	if _, err := os.Stat(catalog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected plan created a catalog: %v", err)
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

func TestLinkedAgentPlanRechecksInputsBeforeAcceptingNoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("initial settings"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		if string(settings.Files[0].Body) != "initial settings" {
			t.Fatalf("unexpected snapshot: %q", settings.Files[0].Body)
		}
		return sdk.AgentPlan{}, os.WriteFile(path, []byte("user settings"), 0o600)
	}
	err := ApplyLinkedAgentPlan(struct{}{}, plan, LinkedActionFile{Name: "settings.json", Path: path})
	if !errors.Is(err, ErrActionChanged) {
		t.Fatalf("accepted a no-write plan based on stale settings: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "user settings" {
		t.Fatalf("user settings changed: %q, %v", got, err)
	}
}

func TestLinkedAgentPlanRechecksFileBytesAndPresence(t *testing.T) {
	body := strings.Repeat("setting\n", 12289)
	for _, fixture := range []struct {
		name   string
		before string
		after  string
		exists bool
		mutate bool
		mode   bool
	}{
		{name: "large unchanged", exists: true, before: body},
		{name: "last byte changed", exists: true, before: body, after: body[:len(body)-1] + "!", mutate: true},
		{name: "prefix truncated", exists: true, before: body, after: body[:len(body)-1], mutate: true},
		{name: "prefix extended", exists: true, before: body, after: body + "!", mutate: true},
		{name: "empty unchanged", exists: true},
		{name: "empty extended", exists: true, after: "!", mutate: true},
		{name: "absent unchanged"},
		{name: "empty file created", mutate: true},
		{name: "same bytes different mode", exists: true, before: body, after: body, mutate: true, mode: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if fixture.mode && runtime.GOOS == "windows" {
				t.Skip("Windows does not distinguish these file modes")
			}
			path := filepath.Join(t.TempDir(), "settings.json")
			if fixture.exists {
				if err := os.WriteFile(path, []byte(fixture.before), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := ApplyLinkedAgentPlan(struct{}{}, func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
				file := settings.Files[0]
				if file.Exists != fixture.exists || string(file.Body) != fixture.before {
					t.Fatal("unexpected settings snapshot")
				}
				if fixture.mode {
					return sdk.AgentPlan{}, os.Chmod(path, 0o640)
				}
				if fixture.mutate {
					return sdk.AgentPlan{}, os.WriteFile(path, []byte(fixture.after), 0o600)
				}
				return sdk.AgentPlan{}, nil
			}, LinkedActionFile{Name: "settings.json", Path: path})
			if !fixture.mutate {
				if err != nil {
					t.Fatalf("unchanged input: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrActionChanged) {
				t.Fatalf("accepted changed input: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != fixture.after {
				t.Fatalf("user file was not preserved: %d bytes, %v", len(got), err)
			}
			if fixture.mode {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o640 {
					t.Fatalf("user file mode was not preserved: %v, %v", info, err)
				}
			}
		})
	}
}

func TestLinkedAgentPlanRechecksRetargetedSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := filepath.Join(dir, "original")
	replacement := filepath.Join(dir, "replacement")
	for _, target := range []string{original, replacement} {
		if err := os.WriteFile(target, []byte("same body"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(original, path); err != nil {
		t.Skipf("symlink: %v", err)
	}
	err := ApplyLinkedAgentPlan(struct{}{}, func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		if string(settings.Files[0].Body) != "same body" {
			t.Fatal("original target was not read")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return sdk.AgentPlan{}, os.Symlink(replacement, path)
	}, LinkedActionFile{Name: "settings.json", Path: path})
	if !errors.Is(err, ErrActionChanged) {
		t.Fatalf("accepted changed symlink target: %v", err)
	}
	if target, err := os.Readlink(path); err != nil || target != replacement {
		t.Fatalf("user symlink was not preserved: %q, %v", target, err)
	}
}

func TestLinkedAgentPlanRestoresRemovedFileAfterRefusal(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "hardlink", "user recreated"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			dotenv := filepath.Join(dir, ".env")
			config := filepath.Join(dir, "settings.json")
			original := dotenv
			if kind == "symlink" || kind == "hardlink" {
				original = filepath.Join(dir, "original")
			}
			if err := os.WriteFile(original, []byte("TOKEN=test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(original, dotenv); err != nil {
					t.Skipf("symlink: %v", err)
				}
			case "hardlink":
				if err := os.Link(original, dotenv); err != nil {
					t.Skipf("hard link: %v", err)
				}
			}
			refused := errors.New("settings write refused")
			plan := func(_ struct{}, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
				env, config := settings.Files[0], settings.Files[1]
				return sdk.AgentPlan{Files: []sdk.FileChange{
					{Path: env.Path, BeforeHash: env.BeforeHash, Remove: true},
					{Path: config.Path, BeforeHash: config.BeforeHash, Body: []byte("new settings")},
				}}, nil
			}
			err := ApplyLinkedAgentPlan(struct{}{}, plan,
				LinkedActionFile{Name: ".env", Path: dotenv},
				LinkedActionFile{Name: "settings.json", Path: config, BeforeWrite: func(string) error {
					body, err := os.ReadFile(dotenv)
					if kind == "hardlink" || kind == "symlink" {
						if err != nil || len(body) != 0 {
							t.Fatalf("linked file was not cleared: %q, %v", body, err)
						}
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("environment file was not removed: %v", err)
					}
					if kind == "user recreated" {
						if err := os.WriteFile(dotenv, []byte("user change\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return refused
				}})
			if !errors.Is(err, refused) {
				t.Fatalf("expected refusal after removing the dotenv file: %v", err)
			}
			want := "TOKEN=test\n"
			if kind == "user recreated" {
				want = "user change\n"
				if !errors.Is(err, ErrActionChanged) {
					t.Fatalf("missed the user's new file: %v", err)
				}
			}
			if got, err := os.ReadFile(dotenv); err != nil || string(got) != want {
				t.Fatalf("after rollback: %q, %v", got, err)
			}
			info, err := os.Stat(dotenv)
			peer, peerErr := os.Stat(original)
			if err != nil || peerErr != nil || !os.SameFile(info, peer) || info.Mode().Perm() != 0o600 {
				t.Fatalf("rollback did not restore mode or link: %v, %v", err, peerErr)
			}
		})
	}
}
