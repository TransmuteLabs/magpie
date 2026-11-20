package extension

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestActionsSessionCreateOnlyPreservesIndependentFiles(test *testing.T) {
	for _, scenario := range []string{"publish-collision", "input-refusal", "rollback-replacement", "commit-replacement"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			policy.Paths, policy.CreateUnder = nil, "sessions"
			directory := filepath.Join(policy.Root, "sessions")
			if err := os.Mkdir(directory, 0o700); err != nil {
				test.Fatal(err)
			}
			first := filepath.Join(directory, "first.jsonl")
			second := filepath.Join(directory, "second.jsonl")
			target := second
			replacesFirst := scenario == "rollback-replacement" || scenario == "commit-replacement"
			if replacesFirst {
				target = first
			}
			output := []byte("converted")
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.SessionResult{Files: []sdk.SessionDocument{
					{Path: "first.jsonl", Format: "target", Body: output},
					{Path: "second.jsonl", Format: "target", Body: output},
				}}, nil
			}
			refused := errors.New("source changed after target validation")
			var independent fs.FileInfo
			installIndependent := func() error {
				competitor := filepath.Join(policy.Root, "independent.jsonl")
				if err := os.WriteFile(competitor, output, 0o600); err != nil {
					return err
				}
				info, err := os.Stat(competitor)
				if err != nil {
					return err
				}
				if err := os.Rename(competitor, target); err != nil {
					return err
				}
				independent = info
				return nil
			}
			policy.RevalidateInputs = func(context.Context, map[string]fs.FileInfo) error {
				if independent != nil || scenario == "commit-replacement" {
					return nil
				}
				if _, err := os.Stat(first); errors.Is(err, fs.ErrNotExist) {
					return nil
				} else if err != nil {
					return err
				}
				if err := installIndependent(); err != nil {
					return err
				}
				if scenario == "publish-collision" {
					return nil
				}
				return refused
			}
			request := sdk.SessionRequest{ID: "conversation", From: "source", To: "target",
				Files: []sdk.SessionDocument{{Path: "input.jsonl", Format: "source", Body: []byte("native")}}}
			plan, err := actions.SessionConvert(test.Context(), "sample", sdk.Scope{}, request, policy)
			if err != nil {
				test.Fatal(err)
			}
			var result ActionResult
			if scenario == "commit-replacement" {
				result, err = plan.ExecuteCreateOnly(test.Context(), plan.Preview().Revision, func(map[string]fs.FileInfo) error {
					return installIndependent()
				})
			} else {
				result, err = plan.Execute(test.Context(), plan.Preview().Revision)
			}
			expected := refused
			if scenario == "publish-collision" || scenario == "commit-replacement" {
				expected = ErrActionChanged
			}
			if independent == nil {
				test.Fatal("competing write did not reach the publication window")
			}
			if info, err := os.Stat(target); err != nil || !os.SameFile(info, independent) {
				test.Errorf("publication or rollback lost the independent file: %v", err)
			} else if string(actionTestRead(test, target)) != string(output) || info.Mode().Perm() != independent.Mode().Perm() {
				test.Error("publication or rollback changed the independent file")
			}
			if result.Applied || !errors.Is(err, expected) {
				test.Errorf("conversion result = %+v, %v; want %v", result, err, expected)
			}
			if !replacesFirst {
				if _, err := os.Stat(first); !errors.Is(err, fs.ErrNotExist) {
					test.Errorf("rollback retained the operation's own output: %v", err)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(target) {
				test.Errorf("unexpected files after rollback: %v, %v", entries, err)
			}
		})
	}
}

func TestActionsSessionCreateOnlyChecksMovedParents(test *testing.T) {
	for _, scenario := range []string{"before-publish", "late-input-refusal", "late-input-success", "success-check"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			policy.Paths, policy.CreateUnder = nil, "sessions"
			directory := filepath.Join(policy.Root, "sessions")
			relocated := filepath.Join(policy.Root, "relocated")
			parent := filepath.Join(directory, "dated")
			output := filepath.Join(parent, "converted.jsonl")
			independentPath := output
			if scenario == "before-publish" {
				independentPath = filepath.Join(parent, "independent.jsonl")
			}
			if err := os.Mkdir(directory, 0o700); err != nil {
				test.Fatal(err)
			}
			payload := []byte("private conversion")
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.SessionResult{Files: []sdk.SessionDocument{
					{Path: "dated/converted.jsonl", Format: "target", Body: payload},
				}}, nil
			}
			var independent fs.FileInfo
			move := func() error {
				if err := os.Rename(directory, relocated); err != nil {
					return err
				}
				if err := os.MkdirAll(parent, 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(independentPath, payload, 0o600); err != nil {
					return err
				}
				var err error
				independent, err = os.Stat(independentPath)
				return err
			}
			refused := errors.New("source changed after publication")
			policy.RevalidateInputs = func(_ context.Context, created map[string]fs.FileInfo) error {
				if independent != nil || scenario == "success-check" || created["sessions/dated"] == nil {
					return nil
				}
				if scenario != "before-publish" {
					if _, err := os.Stat(output); errors.Is(err, fs.ErrNotExist) {
						return nil
					} else if err != nil {
						return err
					}
				}
				if err := move(); err != nil {
					return err
				}
				if scenario == "late-input-refusal" {
					return refused
				}
				return nil
			}
			request := sdk.SessionRequest{ID: "conversation", From: "source", To: "target",
				Files: []sdk.SessionDocument{{Path: "input.jsonl", Format: "source", Body: []byte("native")}}}
			plan, err := actions.SessionConvert(test.Context(), "sample", sdk.Scope{}, request, policy)
			if err != nil {
				test.Fatal(err)
			}
			result, err := plan.ExecuteCreateOnly(test.Context(), plan.Preview().Revision, func(published map[string]fs.FileInfo) error {
				if scenario == "success-check" {
					if published["sessions/dated/converted.jsonl"] == nil {
						return errors.New("success check did not receive the published inode")
					}
					return move()
				}
				return nil
			})
			expected := ErrActionChanged
			if scenario == "late-input-refusal" {
				expected = refused
			}
			if independent == nil {
				test.Fatal("parent did not move during execution")
			}
			if result.Applied || !errors.Is(err, expected) {
				test.Errorf("conversion result = %+v, %v; want %v", result, err, expected)
			}
			if info, err := os.Stat(independentPath); err != nil || !os.SameFile(info, independent) {
				test.Errorf("rollback lost the independent file: %v", err)
			} else if string(actionTestRead(test, independentPath)) != string(payload) || info.Mode().Perm() != independent.Mode().Perm() {
				test.Error("rollback changed the independent file")
			}
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(independentPath) {
				test.Errorf("publication reached the replacement directory: %v, %v", entries, err)
			}
			if entries, err := os.ReadDir(relocated); err != nil || len(entries) != 0 {
				test.Errorf("rollback left a created directory or private files under the moved parent: %v, %v", entries, err)
			}
			if status := string(actionTestRead(test, filepath.Join(result.Backup, "status"))); status != "rolled_back\n" {
				test.Errorf("backup status = %q; want rolled_back", status)
			}
		})
	}
}
