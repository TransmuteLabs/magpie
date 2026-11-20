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

func TestActionsSessionRevisionBindsInputBytesAndOptions(test *testing.T) {
	actions, host, policy := actionFixture(test)
	host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
		return sdk.SessionResult{ID: "converted", Files: []sdk.SessionDocument{
			{Path: "a.json", Format: "target", Body: []byte("same output")},
		}}, nil
	}
	request := sdk.SessionRequest{ID: "conversation", From: "source", To: "target",
		Files: []sdk.SessionDocument{{Path: "selected.jsonl", Format: "source", Body: []byte("first source")}}}
	first, err := actions.SessionConvert(test.Context(), "sample", sdk.Scope{}, request, policy)
	if err != nil {
		test.Fatal(err)
	}
	request.Files[0].Body = []byte("second source")
	second, err := actions.SessionConvert(test.Context(), "sample", sdk.Scope{}, request, policy)
	if err != nil {
		test.Fatal(err)
	}
	request.Options = []byte(`{"selection":"different"}`)
	third, err := actions.SessionConvert(test.Context(), "sample", sdk.Scope{}, request, policy)
	if err != nil {
		test.Fatal(err)
	}
	if first.Preview().Revision == second.Preview().Revision || second.Preview().Revision == third.Preview().Revision {
		test.Fatal("session revision did not bind original input bytes and conversion options")
	}
}

type actionCancelManifestRuntime struct {
	*actionTestRuntime
	remaining int
	cancel    context.CancelFunc
}

func (host *actionCancelManifestRuntime) Manifest(plugin string) (sdk.Manifest, error) {
	manifest, err := host.actionTestRuntime.Manifest(plugin)
	host.remaining--
	if host.remaining == 0 {
		host.cancel()
	}
	return manifest, err
}

func TestActionsCancellationDuringAuthorityCheckCannotPublish(test *testing.T) {
	actions, host, policy := actionFixture(test)
	plan := actionTestPlan(test, actions, policy)
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	actions.host = &actionCancelManifestRuntime{actionTestRuntime: host, remaining: 3, cancel: cancel}
	writes := 0
	actions.write = func(root *os.Root, name string, body []byte, mode fs.FileMode, check func() error) (fs.FileInfo, error) {
		writes++
		return actionWriteChecked(root, name, body, mode, check)
	}
	if _, err := plan.Execute(ctx, plan.Preview().Revision); !errors.Is(err, context.Canceled) {
		test.Fatal("authority cancellation was not returned", err)
	}
	if writes != 0 {
		test.Fatal("cancellation during authority check published a file before rollback", writes)
	}
}

func TestActionsSourceRevalidationUsesApplyContext(test *testing.T) {
	actions, _, policy := actionFixture(test)
	type applyContextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(test.Context(), applyContextKey{}, "apply"))
	defer cancel()
	entered := make(chan struct{})
	policy.RevalidateInputs = func(actual context.Context, _ map[string]fs.FileInfo) error {
		if actual.Value(applyContextKey{}) != "apply" {
			return errors.New("source revalidation lost the apply caller context")
		}
		close(entered)
		<-actual.Done()
		return actual.Err()
	}
	plan := actionTestPlan(test, actions, policy)
	done := make(chan error, 1)
	go func() {
		_, err := plan.Execute(ctx, plan.Preview().Revision)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		test.Fatal("apply did not invoke its source revalidation callback", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		test.Fatal("source revalidation outlived cancellation", err)
	}
	if string(actionTestRead(test, filepath.Join(policy.Root, "a.json"))) != "before-a.json" {
		test.Fatal("cancelled source revalidation published output")
	}
}

func TestActionsSourceRevalidationBeforeEveryPublication(test *testing.T) {
	for _, operation := range []string{"write", "remove"} {
		test.Run(operation, func(test *testing.T) {
			actions, host, policy := actionFixture(test)
			stale := false
			policy.RevalidateInputs = func(ctx context.Context, _ map[string]fs.FileInfo) error {
				if stale {
					return ErrActionChanged
				}
				return ctx.Err()
			}
			host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
				second := sdk.FileChange{Path: "b.json", Body: []byte("after-b")}
				if operation == "remove" {
					second = sdk.FileChange{Path: "b.json", Remove: true}
				}
				return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "a.json", Body: []byte("after-a")}, second}}, nil
			}
			plan := actionTestPlan(test, actions, policy)
			writes := 0
			actions.write = func(root *os.Root, name string, body []byte, mode fs.FileMode, check func() error) (fs.FileInfo, error) {
				writes++
				info, err := actionWriteChecked(root, name, body, mode, check)
				if err != nil {
					return info, err
				}
				stale = true
				return info, nil
			}
			if _, err := plan.Execute(test.Context(), plan.Preview().Revision); !errors.Is(err, ErrActionChanged) {
				test.Fatal("changed source did not stop subsequent publication", err)
			}
			if writes != 1 {
				test.Fatal("another file was published under changed source authority", writes)
			}
			for _, name := range []string{"a.json", "b.json"} {
				if string(actionTestRead(test, filepath.Join(policy.Root, name))) != "before-"+name {
					test.Fatal("source drift did not restore the original target", name)
				}
			}
		})
	}
}
