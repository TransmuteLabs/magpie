package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type actionTestRuntime struct {
	mu       sync.Mutex
	manifest sdk.Manifest
	store    *Store
	scopes   []sdk.Scope
	methods  []string
	invoke   func(context.Context, string, sdk.Scope, any) (any, error)
}

func (host *actionTestRuntime) Manifest(string) (sdk.Manifest, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.manifest.Public(), nil
}

func (host *actionTestRuntime) Store() *Store { return host.store }

func (host *actionTestRuntime) Invoke(ctx context.Context, _ string, method string, scope sdk.Scope, params, out any, _ func(Event) error) error {
	host.mu.Lock()
	host.scopes = append(host.scopes, scope)
	host.methods = append(host.methods, method)
	host.mu.Unlock()
	value, err := host.invoke(ctx, method, scope, params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func actionFixture(test *testing.T) (*Actions, *actionTestRuntime, ActionPolicy) {
	test.Helper()
	root := test.TempDir()
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("before-"+name), 0o640); err != nil {
			test.Fatal(err)
		}
	}
	host := &actionTestRuntime{
		manifest: sdk.Manifest{Schema: 1, ID: "sample", Version: "1.0.0", Contract: sdk.FullContract(),
			Agents:   []sdk.AgentDescriptor{{ID: "editor", Name: "Editor"}},
			Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}}},
		store: NewStore(test.TempDir()),
		invoke: func(context.Context, string, sdk.Scope, any) (any, error) {
			return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "a.json", Body: []byte("after-a")}}}, nil
		},
	}
	policy := ActionPolicy{Root: root, Paths: []string{"a.json", "b.json"}, BackupDir: filepath.Join(test.TempDir(), "backups")}
	return NewActions(host), host, policy
}

func actionTestPlan(test *testing.T, actions *Actions, policy ActionPolicy) *ActionPlan {
	test.Helper()
	plan, err := actions.AgentPlan(context.Background(), "sample", sdk.Scope{}, sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy)
	if err != nil {
		test.Fatal(err)
	}
	return plan
}

func actionTestRead(test *testing.T, name string) []byte {
	test.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		test.Fatal(err)
	}
	return body
}

func TestActionsInspectUsesSnapshotsAndParentAuthority(test *testing.T) {
	actions, host, policy := actionFixture(test)
	scope := sdk.Scope{Plugin: "sample", Session: "parent", Operation: "inspect", Attempt: 7}
	host.invoke = func(_ context.Context, method string, actual sdk.Scope, params any) (any, error) {
		if method != sdk.MethodAgentInspect || actual != scope {
			test.Fatalf("inspection lost authority: %s %+v", method, actual)
		}
		request := params.(sdk.AgentInspectRequest)
		var settings ActionSettings
		if err := json.Unmarshal(request.Settings, &settings); err != nil || len(settings.Files) != 2 ||
			string(settings.Files[0].Body) != "before-a.json" {
			test.Fatalf("inspection trusted caller settings: %s, %v", request.Settings, err)
		}
		return sdk.AgentInspectResult{Connected: true, Values: map[string]string{"model": "external"}}, nil
	}
	result, err := actions.AgentInspect(test.Context(), "sample", scope, sdk.AgentInspectRequest{
		ID: "editor", Settings: json.RawMessage(`{"forged":true}`)}, policy)
	if err != nil || !result.Connected || result.Values["model"] != "external" {
		test.Fatalf("inspection: %+v %v", result, err)
	}
	if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "before-a.json" {
		test.Fatal("inspection wrote files")
	}
	if _, err := os.Stat(policy.BackupDir); !errors.Is(err, fs.ErrNotExist) {
		test.Fatal("inspection created backups")
	}
}

func TestActionsAgentDryRunAndExplicitCommit(test *testing.T) {
	actions, host, policy := actionFixture(test)
	plan := actionTestPlan(test, actions, policy)
	before := actionTestRead(test, filepath.Join(policy.Root, "a.json"))
	if string(before) != "before-a.json" {
		test.Fatal("planning changed the target")
	}
	if _, err := os.Stat(policy.BackupDir); !errors.Is(err, fs.ErrNotExist) {
		test.Fatalf("planning created a backup: %v", err)
	}
	entries, err := os.ReadDir(policy.Root)
	if err != nil || len(entries) != 2 {
		test.Fatalf("planning wrote root metadata: %v, %v", entries, err)
	}
	preview := plan.Preview()
	if len(preview.Files) != 1 || preview.Files[0].BeforeHash != actionHash(actionFile{Body: before, Exists: true}) {
		test.Fatalf("missing host precondition: %+v", preview)
	}
	preview.Files[0].Body[0] = 'X'
	preview.Files[0].Path = "elsewhere"
	snapshots := plan.Snapshots()
	snapshots[0].Body[0] = 'X'
	if string(plan.Snapshots()[0].Body) != "before-a.json" {
		test.Fatal("snapshot exposes mutable preconditions")
	}
	if fresh := plan.Preview(); fresh.Files[0].Path != "a.json" || string(fresh.Files[0].Body) != "after-a" {
		test.Fatal("preview shares mutable plan data")
	}
	if _, err := plan.Execute(context.Background(), "wrong"); !errors.Is(err, ErrActionChanged) {
		test.Fatalf("unconfirmed plan executed: %v", err)
	}
	result, err := plan.Execute(context.Background(), preview.Revision)
	if err != nil || !result.Applied || result.Backup == "" {
		test.Fatalf("commit: %+v, %v", result, err)
	}
	if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "after-a" ||
		string(actionTestRead(test, filepath.Join(policy.Root, "b.json"))) != "before-b.json" {
		test.Fatal("commit changed the wrong targets")
	}
	if !bytes.Equal(actionTestRead(test, filepath.Join(result.Backup, "000.before")), before) {
		test.Fatal("backup did not preserve original bytes")
	}
	if string(actionTestRead(test, filepath.Join(result.Backup, "status"))) != "applied\n" {
		test.Fatal("backup has no terminal status")
	}
	if _, err := plan.Execute(context.Background(), preview.Revision); !errors.Is(err, ErrActionChanged) {
		test.Fatalf("plan was replayed: %v", err)
	}
	if !slices.Equal(host.methods, []string{sdk.MethodAgentPlan}) {
		test.Fatalf("worker was replayed: %v", host.methods)
	}
}

func TestActionsConfirmationBindsIntent(test *testing.T) {
	actions, _, policy := actionFixture(test)
	connect := actionTestPlan(test, actions, policy)
	disconnect, err := actions.AgentPlan(context.Background(), "sample", sdk.Scope{}, sdk.AgentPlanRequest{ID: "editor", Action: "disconnect"}, policy)
	if err != nil {
		test.Fatal(err)
	}
	if connect.Preview().Revision == disconnect.Preview().Revision {
		test.Fatal("confirmation was reusable for another action")
	}
	otherPolicy := policy
	otherPolicy.BackupDir = filepath.Join(test.TempDir(), "other-backups")
	otherBackup := actionTestPlan(test, actions, otherPolicy)
	if connect.Preview().Revision == otherBackup.Preview().Revision {
		test.Fatal("confirmation was reusable with another backup destination")
	}
}

func TestActionsPreserveParentScopeAndScopedState(test *testing.T) {
	actions, host, policy := actionFixture(test)
	account, err := host.store.SaveAccount(Account{Plugin: "sample", Provider: "api", ID: "account", Auth: json.RawMessage(`{"key":"synthetic"}`)}, 0)
	if err != nil {
		test.Fatal(err)
	}
	scope := sdk.Scope{Plugin: "sample", Provider: "api", Account: "account", Generation: account.Generation, Session: "thread", Operation: "parent", Attempt: 7}
	host.invoke = func(_ context.Context, method string, got sdk.Scope, params any) (any, error) {
		if got != scope || method != sdk.MethodAgentPlan {
			test.Fatalf("parent authority changed: %+v, %s", got, method)
		}
		if _, err := host.store.StateCAS(got, "cursor", 0, json.RawMessage(`{"position":1}`)); err != nil {
			return nil, err
		}
		return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "a.json", Body: []byte("after")}}}, nil
	}
	plan, err := actions.AgentPlan(context.Background(), "sample", scope, sdk.AgentPlanRequest{ID: "editor", Action: "set"}, policy)
	if err != nil {
		test.Fatal(err)
	}
	state, err := host.store.StateGet(scope, "cursor")
	if err != nil || state.Version != 1 {
		test.Fatalf("parent-scoped host state: %+v, %v", state, err)
	}
	if err := host.store.DeleteAccount("sample", "api", "account", account.Generation); err != nil {
		test.Fatal(err)
	}
	if _, err := plan.Execute(context.Background(), plan.Preview().Revision); err == nil {
		test.Fatal("revoked account committed a prepared plan")
	}
	if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "before-a.json" {
		test.Fatal("revoked account changed files")
	}
}

func TestActionsAgentReceivesHostSettingsSnapshots(test *testing.T) {
	actions, host, policy := actionFixture(test)
	policy.Paths = append(policy.Paths, "new.json")
	host.invoke = func(_ context.Context, _ string, _ sdk.Scope, params any) (any, error) {
		request := params.(sdk.AgentPlanRequest)
		var settings ActionSettings
		if err := json.Unmarshal(request.Settings, &settings); err != nil {
			return nil, err
		}
		if len(settings.Files) != 3 {
			test.Fatalf("host settings: %+v", settings)
		}
		first, missing := settings.Files[0], settings.Files[2]
		if first.Path != "a.json" || !first.Exists || string(first.Body) != "before-a.json" ||
			first.BeforeHash != actionHash(actionFile{Body: first.Body, Exists: true}) {
			test.Fatalf("host did not supply the actual file: %+v", first)
		}
		if missing.Exists || missing.BeforeHash != MissingActionHash {
			test.Fatalf("missing file was not distinguished: %+v", missing)
		}
		return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "a.json", Body: []byte("after"), BeforeHash: first.BeforeHash}}}, nil
	}
	actionTestPlan(test, actions, policy)
}

func TestActionsRejectUnauthorizedDispatch(test *testing.T) {
	for _, scenario := range []string{"other-plugin", "undeclared-agent", "undeclared-capability", "generation-without-account", "account-without-generation", "cancelled"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			scope := sdk.Scope{}
			request := sdk.AgentPlanRequest{ID: "editor", Action: "connect"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "other-plugin":
				scope.Plugin = "another-owner"
			case "undeclared-agent":
				request.ID = "not-declared"
			case "undeclared-capability":
				host.manifest.Contract.Capabilities = []sdk.Capability{sdk.SessionMigration}
			case "generation-without-account":
				scope.Generation = 1
			case "account-without-generation":
				scope.Provider, scope.Account = "provider", "account"
			case "cancelled":
				cancel()
			}
			if _, err := actions.AgentPlan(ctx, "sample", scope, request, policy); err == nil {
				test.Fatal("unauthorized action was accepted")
			}
			if len(host.methods) != 0 {
				test.Fatal("unauthorized action reached the worker")
			}
		})
	}
}

func TestActionsRejectUnauthorizedOutput(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		files []sdk.FileChange
	}{
		{"traversal", []sdk.FileChange{{Path: "../outside", Body: []byte("bad")}}},
		{"undeclared", []sdk.FileChange{{Path: "other.json"}}},
		{"absolute", []sdk.FileChange{{Path: "/absolute"}}},
		{"reserved", []sdk.FileChange{{Path: ".magpie-extension.lock"}}},
		{"duplicate", []sdk.FileChange{{Path: "a.json"}, {Path: "a.json"}}},
		{"wrong-hash", []sdk.FileChange{{Path: "a.json", BeforeHash: "not-current"}}},
		{"privileged-mode", []sdk.FileChange{{Path: "a.json", Mode: 0o4000}}},
		{"remove-body", []sdk.FileChange{{Path: "a.json", Remove: true, Body: []byte("bad")}}},
		{"too-many", make([]sdk.FileChange, MaxActionFiles+1)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.AgentPlan{Files: scenario.files}, nil
			}
			if _, err := actions.AgentPlan(context.Background(), "sample", sdk.Scope{}, sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy); err == nil {
				test.Fatal("unauthorized worker output was accepted")
			}
			if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "before-a.json" {
				test.Fatal("rejected output changed files")
			}
		})
	}
}

func TestActionsRejectChangedAndMissingSources(test *testing.T) {
	for _, scenario := range []string{"same-metadata", "missing-became-empty", "module-upgraded"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			if scenario == "missing-became-empty" {
				policy.Paths = []string{"new.json"}
				host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
					return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "new.json", Body: []byte("after")}}}, nil
				}
			}
			plan := actionTestPlan(test, actions, policy)
			switch scenario {
			case "same-metadata":
				name := filepath.Join(policy.Root, "a.json")
				info, err := os.Stat(name)
				if err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("edited-a.json"), info.Mode().Perm()); err != nil {
					test.Fatal(err)
				}
				if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
					test.Fatal(err)
				}
			case "missing-became-empty":
				if plan.Preview().Files[0].BeforeHash != MissingActionHash {
					test.Fatal("missing and empty snapshots were conflated")
				}
				if err := os.WriteFile(filepath.Join(policy.Root, "new.json"), nil, 0o600); err != nil {
					test.Fatal(err)
				}
			case "module-upgraded":
				host.manifest.Version = "2.0.0"
			}
			if _, err := plan.Execute(context.Background(), plan.Preview().Revision); !errors.Is(err, ErrActionChanged) {
				test.Fatalf("stale plan committed: %v", err)
			}
		})
	}
}

func TestActionsRollbackAfterPartialFailure(test *testing.T) {
	for _, cancelAfterFirst := range []bool{false, true} {
		test.Run(map[bool]string{false: "write-failure", true: "cancellation"}[cancelAfterFirst], func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "a.json", Body: []byte("after-a")}, {Path: "b.json", Body: []byte("after-b")}}}, nil
			}
			plan := actionTestPlan(test, actions, policy)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writes := 0
			actions.write = func(root *os.Root, name string, body []byte, mode fs.FileMode) error {
				writes++
				if !cancelAfterFirst && writes == 2 {
					return errors.New("injected second write failure")
				}
				err := actionWrite(root, name, body, mode)
				if cancelAfterFirst && writes == 1 {
					cancel()
				}
				return err
			}
			result, err := plan.Execute(ctx, plan.Preview().Revision)
			if err == nil || result.Applied || result.Backup == "" {
				test.Fatalf("failed transaction: %+v, %v", result, err)
			}
			if cancelAfterFirst && !errors.Is(err, context.Canceled) {
				test.Fatalf("cancellation was lost: %v", err)
			}
			for _, name := range policy.Paths {
				if string(actionTestRead(test, filepath.Join(policy.Root, name))) != "before-"+name {
					test.Fatalf("%s was not restored", name)
				}
			}
			if string(actionTestRead(test, filepath.Join(result.Backup, "status"))) != "rolled_back\n" {
				test.Fatal("rollback was not recorded")
			}
		})
	}
}

func TestActionsPreserveConcurrentApplicationEdits(test *testing.T) {
	for _, duringRollback := range []bool{false, true} {
		test.Run(map[bool]string{false: "next-target", true: "rollback-target"}[duringRollback], func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "a.json", Body: []byte("after-a")}, {Path: "b.json", Body: []byte("after-b")}}}, nil
			}
			plan := actionTestPlan(test, actions, policy)
			actions.write = func(root *os.Root, name string, body []byte, mode fs.FileMode) error {
				if duringRollback && name == "b.json" {
					if err := root.WriteFile("a.json", []byte("application-edit"), 0o640); err != nil {
						return err
					}
					return errors.New("injected later failure")
				}
				if err := actionWrite(root, name, body, mode); err != nil {
					return err
				}
				if !duringRollback && name == "a.json" {
					return root.WriteFile("b.json", []byte("application-edit"), 0o640)
				}
				return nil
			}
			result, err := plan.Execute(context.Background(), plan.Preview().Revision)
			if !errors.Is(err, ErrActionChanged) || result.Backup == "" || result.Applied {
				test.Fatalf("concurrent edit: %+v, %v", result, err)
			}
			target := "b.json"
			if duringRollback {
				target = "a.json"
			}
			if string(actionTestRead(test, filepath.Join(policy.Root, target))) != "application-edit" {
				test.Fatal("the application's edit was overwritten")
			}
			if string(actionTestRead(test, filepath.Join(result.Backup, "000.before"))) != "before-a.json" {
				test.Fatal("original bytes were not retained for recovery")
			}
		})
	}
}

func TestActionsRollbackNewFilesAndRemovals(test *testing.T) {
	actions, host, policy := actionFixture(test)
	policy.Paths = []string{"a.json", "b/new.json", "c.json"}
	host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
		return sdk.AgentPlan{Files: []sdk.FileChange{
			{Path: "a.json", Remove: true},
			{Path: "b/new.json", Body: []byte("created")},
			{Path: "c.json", Body: []byte("fail")},
		}}, nil
	}
	plan := actionTestPlan(test, actions, policy)
	actions.write = func(root *os.Root, name string, body []byte, mode fs.FileMode) error {
		if name == "c.json" {
			return errors.New("injected final write failure")
		}
		return actionWrite(root, name, body, mode)
	}
	result, err := plan.Execute(context.Background(), plan.Preview().Revision)
	if err == nil || result.Applied {
		test.Fatalf("failed transaction: %+v, %v", result, err)
	}
	if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "before-a.json" {
		test.Fatal("removed original was not restored")
	}
	for _, name := range []string{"b", "c.json"} {
		if _, err := os.Stat(filepath.Join(policy.Root, name)); !errors.Is(err, fs.ErrNotExist) {
			test.Fatalf("failed transaction left %s: %v", name, err)
		}
	}
	if string(actionTestRead(test, filepath.Join(result.Backup, "status"))) != "rolled_back\n" {
		test.Fatal("rollback did not reach its terminal state")
	}
}

func TestActionsSessionConversionIsBoundedAndExplicit(test *testing.T) {
	actions, host, policy := actionFixture(test)
	policy.Paths = []string{"new/session.jsonl"}
	input := []byte(`{"native":"source"}`)
	host.invoke = func(_ context.Context, method string, scope sdk.Scope, params any) (any, error) {
		request := params.(sdk.SessionRequest)
		if method != sdk.MethodSessionConvert || scope.Session != "source-id" || !bytes.Equal(request.Files[0].Body, input) {
			test.Fatal("conversion input or parent session was changed")
		}
		return sdk.SessionResult{Files: []sdk.SessionDocument{{Path: "new/session.jsonl", Format: "target", Body: []byte(`{"native":"target"}`)}}}, nil
	}
	request := sdk.SessionRequest{ID: "conversation", From: "source", To: "target", Files: []sdk.SessionDocument{{Path: "source.jsonl", Format: "source", Body: input}}}
	plan, err := actions.SessionConvert(context.Background(), "sample", sdk.Scope{Session: "source-id"}, request, policy)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(policy.Root, "new")); !errors.Is(err, fs.ErrNotExist) {
		test.Fatal("conversion wrote its target before confirmation")
	}
	result, err := plan.Execute(context.Background(), plan.Preview().Revision)
	if err != nil || !result.Applied {
		test.Fatalf("conversion commit: %+v, %v", result, err)
	}
	if string(actionTestRead(test, filepath.Join(policy.Root, "new", "session.jsonl"))) != `{"native":"target"}` ||
		string(input) != `{"native":"source"}` {
		test.Fatal("conversion changed the source or lost the target")
	}
	request.Files[0].Body = make([]byte, MaxActionBytes+1)
	if _, err := actions.SessionConvert(context.Background(), "sample", sdk.Scope{}, request, policy); err == nil {
		test.Fatal("oversized session input was accepted")
	}
	if len(host.methods) != 1 {
		test.Fatal("invalid input reached the worker")
	}
}

func TestActionsSessionOutputFormatAndOwnership(test *testing.T) {
	for _, scenario := range []string{"other-format", "other-path", "duplicate-input", "undeclared-converter"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			request := sdk.SessionRequest{ID: "conversation", From: "source", To: "target",
				Files: []sdk.SessionDocument{{Path: "input", Format: "source", Body: []byte("native")}}}
			output := sdk.SessionDocument{Path: "a.json", Format: "target", Body: []byte("converted")}
			switch scenario {
			case "other-format":
				output.Format = "not-selected"
			case "other-path":
				output.Path = "../outside"
			case "duplicate-input":
				request.Files = append(request.Files, request.Files[0])
			case "undeclared-converter":
				request.ID = "not-declared"
			}
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.SessionResult{Files: []sdk.SessionDocument{output}}, nil
			}
			if _, err := actions.SessionConvert(context.Background(), "sample", sdk.Scope{}, request, policy); err == nil {
				test.Fatal("invalid conversion was accepted")
			}
			if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "before-a.json" {
				test.Fatal("invalid conversion changed the target")
			}
		})
	}
}

func TestActionsSessionCreateOnlyPrefix(test *testing.T) {
	for _, scenario := range []string{"new", "already-present", "appeared-after-preview", "traversal"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			policy.Paths, policy.CreateUnder = nil, "sessions/external"
			output := "project/session.jsonl"
			if scenario == "traversal" {
				output = "../escape"
			}
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				return sdk.SessionResult{Files: []sdk.SessionDocument{{Path: output, Format: "target", Body: []byte("converted")}}}, nil
			}
			target := filepath.Join(policy.Root, "sessions", "external", "project", "session.jsonl")
			createExisting := func() {
				test.Helper()
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("keep-existing"), 0o600); err != nil {
					test.Fatal(err)
				}
			}
			if scenario == "already-present" {
				createExisting()
			}
			request := sdk.SessionRequest{ID: "conversation", From: "source", To: "target",
				Files: []sdk.SessionDocument{{Path: "source.jsonl", Format: "source", Body: []byte("native")}}}
			plan, err := actions.SessionConvert(context.Background(), "sample", sdk.Scope{}, request, policy)
			if scenario == "already-present" || scenario == "traversal" {
				if err == nil {
					test.Fatal("unsafe session creation was accepted")
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			preview := plan.Preview()
			if len(preview.Files) != 1 || preview.Files[0].Path != "sessions/external/project/session.jsonl" ||
				preview.Files[0].BeforeHash != MissingActionHash {
				test.Fatalf("output was not confined: %+v", preview)
			}
			if scenario == "appeared-after-preview" {
				createExisting()
			}
			result, err := plan.Execute(context.Background(), preview.Revision)
			if scenario == "appeared-after-preview" {
				if !errors.Is(err, ErrActionChanged) || string(actionTestRead(test, target)) != "keep-existing" {
					test.Fatalf("collision was overwritten: %+v, %v", result, err)
				}
				return
			}
			if err != nil || !result.Applied || string(actionTestRead(test, target)) != "converted" {
				test.Fatalf("create-only conversion failed: %+v, %v", result, err)
			}
		})
	}
}

func TestActionsRejectSymlinkTargets(test *testing.T) {
	for _, late := range []bool{false, true} {
		test.Run(map[bool]string{false: "before-plan", true: "before-commit"}[late], func(test *testing.T) {
			actions, _, policy := actionFixture(test)
			outside := filepath.Join(test.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				test.Fatal(err)
			}
			var plan *ActionPlan
			if late {
				plan = actionTestPlan(test, actions, policy)
			}
			target := filepath.Join(policy.Root, "a.json")
			if err := os.Remove(target); err != nil {
				test.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				test.Skipf("symlink creation unavailable: %v", err)
			}
			var err error
			if late {
				_, err = plan.Execute(context.Background(), plan.Preview().Revision)
			} else {
				_, err = actions.AgentPlan(context.Background(), "sample", sdk.Scope{}, sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy)
			}
			if err == nil || string(actionTestRead(test, outside)) != "untouched" {
				test.Fatalf("symlink target was accepted: %v", err)
			}
		})
	}
}

func TestActionsConcurrentPlansPublishOnce(test *testing.T) {
	actions, host, policy := actionFixture(test)
	other := NewActions(host)
	plans := []*ActionPlan{actionTestPlan(test, actions, policy), actionTestPlan(test, other, policy)}
	start := make(chan struct{})
	results := make(chan error, len(plans))
	for _, plan := range plans {
		go func() {
			<-start
			_, err := plan.Execute(context.Background(), plan.Preview().Revision)
			results <- err
		}()
	}
	close(start)
	published := 0
	for range plans {
		if err := <-results; err == nil {
			published++
		} else if !errors.Is(err, ErrActionChanged) {
			test.Errorf("unexpected concurrent error: %v", err)
		}
	}
	if published != 1 {
		test.Fatalf("%d plans committed, want one", published)
	}
}
