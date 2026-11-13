package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shellPlanFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	first, second := filepath.Join(home, ".first"), filepath.Join(home, ".second")
	beforeProfiles, beforeProgram := fnProfiles, cliExe
	fnProfiles = func() []ShellProfile {
		return []ShellProfile{{Path: first, Kind: "posix", Create: true}, {Path: second, Kind: "posix", Create: true}}
	}
	cliExe = func() (string, error) { return "/host/magpie", nil }
	t.Cleanup(func() { fnProfiles, cliExe = beforeProfiles, beforeProgram })
	return first, second
}

func TestShellFunctionPlanPreservesOutsideEditsAndRestoresOwnedBlock(t *testing.T) {
	first, _ := shellPlanFixture(t)
	original := withFnBlock("personal\n", "editor", "old body", false)
	if err := os.WriteFile(first, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanShellFunction("editor", func(string, string) string { return "new body" })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(first)
	if string(raw) != original {
		t.Fatal("shell planning wrote a profile")
	}
	if err := os.WriteFile(first, append(raw, []byte("later personal\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(plan.Revision()); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(first)
	if !strings.Contains(string(raw), "new body") || !strings.Contains(string(raw), "later personal") {
		t.Fatal("shell owner lost the update or unrelated user text")
	}
	if err := RestoreShellFunctions(plan.Snapshots()); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(first)
	if !strings.Contains(string(raw), "old body") || strings.Contains(string(raw), "new body") ||
		!strings.Contains(string(raw), "later personal") {
		t.Fatal("shell recovery did not preserve the user's outside edits")
	}
}

func TestShellFunctionPlanRollsBackEarlierProfileOnDrift(t *testing.T) {
	first, second := shellPlanFixture(t)
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte(withFnBlock("personal\n", "editor", "old", false)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(first)
	plan, err := PlanShellFunction("editor", func(string, string) string { return "new" })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(withFnBlock("personal\n", "editor", "changed by user", false)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(plan.Revision()); err == nil {
		t.Fatal("stale shell plan overwrote user drift")
	}
	after, _ := os.ReadFile(first)
	if string(after) != string(before) {
		t.Fatal("failed shell transition left an earlier profile changed")
	}
	after, _ = os.ReadFile(second)
	if !strings.Contains(string(after), "changed by user") {
		t.Fatal("failed shell transition discarded the user's changed block")
	}
}

func TestShellFunctionPlanRestoresMissingProfileAndRejectsChangedBlock(t *testing.T) {
	first, _ := shellPlanFixture(t)
	plan, err := PlanShellFunction("editor", func(string, string) string { return "new" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(plan.Revision()); err != nil {
		t.Fatal(err)
	}
	if err := RestoreShellFunctions(plan.Snapshots()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shell recovery manufactured an empty profile")
	}
	if _, err := plan.Apply(plan.Revision()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte(withFnBlock("", "editor", "user body", false)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreShellFunctions(plan.Snapshots()); err == nil {
		t.Fatal("shell recovery overwrote a user-edited owned block")
	}
}

func TestShellFunctionPlanFailedRollbackRemainsOwnedAndDetectable(t *testing.T) {
	first, second := shellPlanFixture(t)
	plan, err := PlanShellFunction("editor", func(string, string) string { return "new" })
	if err != nil {
		t.Fatal(err)
	}
	applyFailure, rollbackFailure := errors.New("second profile unavailable"), errors.New("first rollback unavailable")
	plan.write = func(snapshot ShellFunctionSnapshot, restore bool) error {
		if snapshot.Path == second {
			return applyFailure
		}
		if snapshot.Path == first && restore {
			return rollbackFailure
		}
		return writeShellFunctionSnapshot(snapshot, restore)
	}
	_, err = plan.Apply(plan.Revision())
	if !errors.Is(err, applyFailure) || !errors.Is(err, rollbackFailure) {
		t.Fatal("partial rollback failure was not surfaced", err)
	}
	if err := ShellFunctionsMatch(plan.Snapshots(), false); err == nil {
		t.Fatal("failed rollback was reported as restored")
	}
	if err := ShellFunctionsRecoverable(plan.Snapshots()); err != nil {
		t.Fatal("failed rollback lost the known shell preimages", err)
	}
	if err := RestoreShellFunctions(plan.Snapshots()); err != nil {
		t.Fatal(err)
	}
	if err := ShellFunctionsMatch(plan.Snapshots(), false); err != nil {
		t.Fatal("explicit recovery did not prove complete restoration", err)
	}
}
