package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestClassifierParentDeadlineDoesNotRestFreshTurn(t *testing.T) {
	fixture := newClassifierScopeFixture(t, true)
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	finished := make(chan *httptest.ResponseRecorder, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		finished <- classifierScopeFront(parent, fixture.server, fixture.secrets[0], "scope", "parent-deadline", "let this parent deadline expire")
	}()
	t.Cleanup(func() {
		cancel()
		fixture.once.Do(func() { close(fixture.release) })
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("parent deadline request did not join during cleanup")
		}
	})
	select {
	case account := <-fixture.started:
		if account != "scope-a" {
			t.Fatalf("deadline selected %q", account)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadline fixture did not dispatch")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("parent deadline did not join")
	}
	if parent.Err() != context.DeadlineExceeded || !slices.Equal(fixture.took(), []string{"scope-a"}) {
		t.Fatalf("wrong deadline owner or send count: %v %v", parent.Err(), fixture.took())
	}
	fixture.once.Do(func() { close(fixture.release) })
	fresh, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	second := classifierScopeFront(fresh, fixture.server, fixture.secrets[0], "scope", "fresh-after-deadline", "fresh question after parent deadline")
	if second.Code != 200 || !slices.Equal(fixture.took(), []string{"scope-a", "scope-a"}) {
		t.Fatalf("parent deadline rested a fresh same-account question: status=%d sends=%v", second.Code, fixture.took())
	}
	third := classifierScopeFront(fresh, fixture.server, fixture.secrets[0], "scope", "rest-real-refusal", "another question after actual upstream refusal")
	route := lastRoute(fixture.server)
	if third.Code != 200 || !slices.Equal(fixture.took(), []string{"scope-a", "scope-a"}) ||
		route.Rule == nil || route.Rule.Classified == nil || !strings.Contains(route.Rule.Classified.Error, "not asked again") {
		t.Fatalf("genuine refusal no longer rests after deadline recovery: status=%d sends=%v route=%+v", third.Code, fixture.took(), route.Rule)
	}
}

func TestClassifierOwnTimeoutStillRestsAccount(t *testing.T) {
	fixture := newClassifierScopeFixture(t, true)
	current := settings.Load()
	current.ClassifyTimeout = 1
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	outer, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := classifierScopeFront(outer, fixture.server, fixture.secrets[0], "scope", "own-timeout", "wait for the classifier child timeout")
	if outer.Err() != nil || first.Code != 200 || !slices.Equal(fixture.took(), []string{"scope-a"}) {
		t.Fatalf("classifier child timeout lost its live outer request: outer=%v status=%d sends=%v", outer.Err(), first.Code, fixture.took())
	}
	fixture.once.Do(func() { close(fixture.release) })
	second := classifierScopeFront(outer, fixture.server, fixture.secrets[0], "scope", "rest-own-timeout", "fresh question after classifier timeout")
	route := lastRoute(fixture.server)
	if second.Code != 200 || !slices.Equal(fixture.took(), []string{"scope-a"}) ||
		route.Rule == nil || route.Rule.Classified == nil || !strings.Contains(route.Rule.Classified.Error, "not asked again") {
		t.Fatalf("classifier-owned timeout stopped resting: status=%d sends=%v route=%+v", second.Code, fixture.took(), route.Rule)
	}
	var records []usage.Record
	for _, record := range usage.Load(time.Time{}) {
		if record.Provider == "scope-judge" {
			records = append(records, record)
		}
	}
	if len(records) != 1 || !records[0].Failed() || usage.CanceledBy(records[0].Status, records[0].Error) {
		t.Fatalf("classifier-owned deadline changed attribution: %+v", records)
	}
}

func TestClassifierTerminalOutcomeRetainsRestAfterParentCancel(t *testing.T) {
	directory := buildDecisionFixture(t, "exec")
	for _, scenario := range []struct {
		outcome string
		mode    string
	}{
		{outcome: "accepted", mode: "accepted error"},
		{outcome: "uncertain", mode: "uncertain"},
		{outcome: "completed", mode: "typed-completed-control"},
	} {
		t.Run(scenario.outcome, func(t *testing.T) {
			manager, id := installDecisionFixture(t, directory, scenario.mode)
			resetClassified(t)
			server := New()
			t.Cleanup(server.idle.stop)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			accounts, err := access.CleanAccounts([]string{id + "/alice"})
			if err != nil {
				t.Fatal(err)
			}
			held := access.Identity{Accounts: accounts}
			callContext := ctx
			var original error
			calls := 0
			ask := func(who access.Identity, model string, intents []string, previous before, effort bool, text string) (verdict, error) {
				calls++
				if scenario.outcome == "completed" {
					original = &decisionOperationError{
						cause:  &extensionhost.OutcomeError{Outcome: "completed"},
						module: "fixture.decision", operation: "synthetic-completed-control",
					}
					cancel()
					return verdict{}, original
				}
				result, err := server.askClassifierWithContext(access.WithIdentity(callContext, who), 5*time.Second, model, intents, previous, effort, text)
				original = err
				cancel()
				return result, err
			}
			_, _, first := classifyContext(ctx, ask, held, id+"/decision", []string{"bug"}, before{}, false, "the first SDK classifier question")
			var owner *decisionOperationError
			if first != original || ctx.Err() == nil || !terminalExtensionError(first) ||
				!errors.As(first, &owner) || owner.module == "" || owner.operation == "" {
				t.Fatalf("canceled owner lost its exact terminal error/receipt: err=%v original=%v receipt=%+v", first, original, owner)
			}
			var rpc *sdk.Error
			var outcome *extensionhost.OutcomeError
			got := ""
			if errors.As(first, &rpc) {
				got = rpc.Outcome
			} else if errors.As(first, &outcome) {
				got = outcome.Outcome
			}
			if got != scenario.outcome {
				t.Fatalf("fixture outcome=%q, want %s: %v", got, scenario.outcome, first)
			}
			callContext = t.Context()
			_, _, second := classifyContext(t.Context(), ask, held, id+"/decision", []string{"bug"}, before{}, false, "another SDK classifier question")
			var shared *decisionOperationError
			if calls != 1 || !terminalExtensionError(second) || !errors.As(second, &shared) ||
				shared.module != "" || shared.operation != "" {
				t.Fatalf("cancellation erased shared terminal rest or leaked receipt: calls=%d err=%v receipt=%+v", calls, second, shared)
			}
			wantExecutions := 1
			if scenario.outcome == "completed" {
				wantExecutions = 0
			}
			if len(decisionExecutions(t, manager, "alice")) != wantExecutions || len(decisionExecutions(t, manager, "bob")) != 0 {
				t.Fatal("terminal classifier was replayed or escaped selected account")
			}
		})
	}
}
