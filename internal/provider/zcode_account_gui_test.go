package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func TestZCodeAccountChoicesUnavailableWithoutProviderListing(t *testing.T) {
	claudeHome(t)
	plugin.UseCached(nil)
	t.Cleanup(func() { plugin.UseCached(nil) })
	choices, available := ZCodeAccountChoices()
	if !available || len(choices) != 0 {
		t.Fatalf("intrinsic empty account discovery: available=%v choices=%+v", available, choices)
	}
}

func TestZCodeAccountChoicesFollowCurrentLocalAccounts(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode":   {"type": "oauth", "accountId": "same@example.test", "refresh": "secret-a"},
		"zcode#2": {"type": "oauth", "accountId": "same@example.test", "refresh": "secret-b"},
	})
	choices, available := ZCodeAccountChoices()
	if !available || len(choices) != 2 {
		t.Fatalf("choices=%+v available=%v", choices, available)
	}
	users := map[string]string{}
	for _, choice := range choices {
		users[choice.Account] = choice.User
		if choice.Provider != PluginID("zcode") || choice.PluginProvider != "zcode" || zcodeAccountSelector(choice.Provider, choice.Account) != choice.User {
			t.Fatalf("choice cannot route its own account: %+v", choice)
		}
	}
	if users["zcode"] == "" || users["zcode#2"] == "" || strings.EqualFold(users["zcode"], users["zcode#2"]) {
		t.Fatalf("duplicate names are not uniquely routed: %v", users)
	}
	if b, err := json.Marshal(choices); err != nil || strings.Contains(string(b), "secret-") {
		t.Fatalf("credentials exposed: %s, %v", b, err)
	}
	auth, _ := json.Marshal(map[string]any{
		"zcode":   map[string]any{"type": "oauth", "accountId": "same@example.test", "refresh": "secret-a"},
		"zcode#3": map[string]any{"type": "oauth", "accountId": "third@example.test", "refresh": "secret-c"},
	})
	if err := os.WriteFile(plugin.AuthPath(), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	choices, available = ZCodeAccountChoices()
	if !available || len(choices) != 2 {
		t.Fatalf("fresh choices=%+v available=%v", choices, available)
	}
	for _, choice := range choices {
		if choice.Account == "zcode#2" {
			t.Fatalf("removed account still listed: %+v", choices)
		}
	}
	if err := plugin.SetOff("opencode-zcode-auth", true); err != nil {
		t.Fatal(err)
	}
	choices, available = ZCodeAccountChoices()
	if !available || len(choices) != 2 {
		t.Fatalf("external plugin toggle changed intrinsic choices=%+v available=%v", choices, available)
	}
}

func TestZCodeAccountChoicesPreserveLoginsWhenSnapshotsFail(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    bool
		directory bool
		sameStamp bool
		bad       string
	}{
		{name: "auth unreadable", directory: true},
		{name: "auth malformed with backup", bad: `{"zcode":`},
		{name: "auth malformed same stamp", sameStamp: true},
		{name: "auth invalid account", bad: `{"zcode":{"type":false}}`},
		{name: "auth missing account type", bad: `{"zcode":{}}`},
		{name: "config unreadable", config: true, directory: true},
		{name: "config malformed with backup", config: true, bad: `{"plugins":`},
		{name: "config invalid field", config: true, bad: `{"plugins":[{"spec":"opencode-zcode-auth","off":"invalid"}]}`},
		{name: "config null entry", config: true, bad: `{"plugins":[null]}`},
		{name: "config null off flag", config: true, bad: `{"plugins":[{"spec":"opencode-zcode-auth","off":null}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			movedPlugin(t, "zcode", map[string]map[string]any{
				"zcode":   {"type": "oauth", "accountId": "one@example.test"},
				"zcode#2": {"type": "oauth", "accountId": "two@example.test"},
				"zcode#3": {"type": "oauth", "accountId": "hidden@example.test"},
			})
			if err := writeLogins([]savedLogin{
				{Agent: "plugin:zcode", Home: "zcode#2", User: "two@example.test", Order: 1, On: false},
				{Agent: "plugin:zcode", Home: "zcode", User: "one@example.test", Order: 2, On: true},
				{Agent: "plugin:zcode", Home: "zcode#3", User: "hidden@example.test", Order: 3, Hidden: "hidden-mark"},
			}); err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().Add(-10 * time.Second)
			if tc.sameStamp {
				if err := os.Chtimes(plugin.AuthPath(), stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if choices, available := ZCodeAccountChoices(); !available || len(choices) != 2 {
				t.Fatalf("initial choices=%+v available=%v", choices, available)
			}
			before, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			path := plugin.AuthPath()
			if tc.config {
				path = plugin.ListPath()
			}
			good, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".bak", good, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.directory {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				bad := []byte(tc.bad)
				if tc.sameStamp {
					bad = bytes.Clone(good)
					bad[0] = '!'
				}
				if err := os.WriteFile(path, bad, 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.sameStamp {
					if err := os.Chtimes(path, stamp, stamp); err != nil {
						t.Fatal(err)
					}
				}
			}
			if choices, available := ZCodeAccountChoices(); available || len(choices) != 0 {
				t.Fatalf("failed snapshot presented as available: choices=%+v available=%v", choices, available)
			}
			after, err := os.ReadFile(loginsPath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed snapshot rewrote routing rows: before=%s after=%s err=%v", before, after, err)
			}
			if tc.directory {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, good, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.sameStamp {
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if choices, available := ZCodeAccountChoices(); !available || len(choices) != 2 {
				t.Fatalf("recovered choices=%+v available=%v", choices, available)
			}
			after, err = os.ReadFile(loginsPath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("recovery changed off/order/hidden rows: before=%s after=%s err=%v", before, after, err)
			}
		})
	}
}

func TestZCodeAccountChoicesTreatMissingSnapshotsAsKnownEmpty(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode": {"type": "oauth", "accountId": "one@example.test"},
	})
	if err := os.Remove(plugin.AuthPath()); err != nil {
		t.Fatal(err)
	}
	if choices, available := ZCodeAccountChoices(); !available || len(choices) != 0 {
		t.Fatalf("missing auth choices=%+v available=%v", choices, available)
	}
	if err := os.Remove(plugin.ListPath()); err != nil {
		t.Fatal(err)
	}
	if choices, available := ZCodeAccountChoices(); !available || len(choices) != 0 {
		t.Fatalf("missing config choices=%+v available=%v", choices, available)
	}
}

func TestZCodeSelectionUsesSelectedSlotCardUser(t *testing.T) {
	c := zcodeTestContext(t)
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode": {"type": "oauth", "accountId": "same@example.test"},
	})
	selection := c
	selection.Scope, selection.Generation = "team-scope", "team-generation"
	selection.Kind, selection.Organization, selection.Project, selection.Product = "team", "org", "project", "product"
	selection.UserID = "vendor-user-id"
	selectedSlot := "zcode#2"
	zcodeTestRPC(t, func(request plugin.AccountRequest) (plugin.AccountResult, error) {
		if request.Operation == "context" {
			return zcodeTestData(t, c, nil), nil
		}
		if request.Operation != "selection.select" || request.Scope != c.Scope || request.Generation != c.Generation {
			t.Fatalf("unexpected operation: %+v", request)
		}
		auth, err := json.Marshal(map[string]any{
			"zcode":   map[string]any{"type": "oauth", "accountId": "same@example.test"},
			"zcode#2": map[string]any{"type": "oauth", "accountId": "same@example.test"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plugin.AuthPath(), auth, 0o600); err != nil {
			t.Fatal(err)
		}
		return zcodeTestData(t, selection, map[string]any{"account": selectedSlot, "user": "same@example.test", "created": true}), nil
	})
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	result, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "selection.select", nil)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Account string `json:"account"`
		User    string `json:"user"`
		Created bool   `json:"created"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	choices, available := ZCodeAccountChoices()
	var cardUser string
	for _, choice := range choices {
		if choice.Account == data.Account {
			cardUser = choice.User
		}
	}
	if !available || data.Account != "zcode#2" || data.User != cardUser || data.User != "same@example.test (2)" || !data.Created || result.Context.UserID != "vendor-user-id" {
		t.Fatalf("selection=%+v card user=%q context=%+v", data, cardUser, result.Context)
	}
	selectedSlot = "zcode#missing"
	result, err = ZCodeAccountOperation(ctx, "zcode", "zcode", "selection.select", nil)
	if err != nil || json.Unmarshal(result.Data, &data) != nil || data.User != "" {
		t.Fatalf("unresolved slot reused the raw account ID: data=%s err=%v", result.Data, err)
	}
}

func TestZCodeAccountSelectorDeclinesActualUserAndTraceCollision(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode":   {"type": "oauth", "accountId": "alice@example.test", "refresh": "secret-a"},
		"zcode#2": {"type": "oauth", "accountId": "alice%40example.test", "refresh": "secret-b"},
		"zcode#3": {"type": "oauth", "accountId": "zcode@alice%40example.test", "refresh": "secret-c"},
		"zcode#4": {"type": "oauth", "accountId": "zcode", "refresh": "secret-d"},
	})
	if got := zcodeAccountSelector("zcode", "zcode"); got != "alice@example.test" {
		t.Fatalf("unrelated percent names hid the unambiguous selector: %q", got)
	}
	// Default sorting makes alice%40... primary. Its user collides after
	// decoding, and the primary trace ID also matches the fourth user.
	if got := zcodeAccountSelector("zcode", "zcode#2"); got != "" {
		t.Fatalf("user and trace ID both match another account: %q", got)
	}
}

func TestZCodeAccountFenceRejectsStaleBeforeCoordinatorDispatch(t *testing.T) {
	c := zcodeTestContext(t)
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode": {"type": "oauth", "accountId": "alice", "refresh": "secret"},
	})
	assertRefusal := func(err error) {
		t.Helper()
		var accountErr *plugin.AccountError
		if !errors.As(err, &accountErr) || accountErr.Dispatched || !accountErr.DispatchedKnown {
			t.Fatalf("preflight did not report known non-dispatch: %v", err)
		}
		b, marshalErr := json.Marshal(accountErr)
		var wire map[string]any
		if marshalErr != nil || json.Unmarshal(b, &wire) != nil || wire["dispatched"] != false {
			t.Fatalf("preflight refusal lost dispatched:false: %s %v", b, marshalErr)
		}
	}
	var operations []string
	zcodeTestRPC(t, func(request plugin.AccountRequest) (plugin.AccountResult, error) {
		operations = append(operations, request.Operation)
		if request.Operation != "context" {
			return plugin.AccountResult{}, errors.New("unexpected vendor operation")
		}
		return zcodeTestData(t, c, nil), nil // also tests Go's post-capture check
	})
	for _, expectation := range [][2]string{{"old-scope", c.Generation}, {c.Scope, "old-generation"}} {
		ctx := WithZCodeAccountExpectation(context.Background(), expectation[0], expectation[1])
		_, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "execution.draw", map[string]string{"taskId": "chat", "model": "GLM-5.3"})
		assertRefusal(err)
		_, err = UseZCodeReset(ctx, "zcode", "zcode", "week")
		assertRefusal(err)
	}
	for _, operation := range operations {
		if operation != "context" {
			t.Fatalf("stale operation reached vendor: %v", operations)
		}
	}
	for _, kind := range []string{"account_changed", "account_scope_changed", "account_generation_changed", "scope_changed"} {
		original := &plugin.AccountError{Kind: kind, Dispatched: true, DispatchedKnown: true}
		zcodeTestRPC(t, func(request plugin.AccountRequest) (plugin.AccountResult, error) {
			if request.Operation != "context" {
				t.Fatalf("preflight dispatched %s", request.Operation)
			}
			return plugin.AccountResult{}, original
		})
		ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
		_, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim", map[string]string{"plan_id": "gift"})
		assertRefusal(err)
		if !original.Dispatched || !original.DispatchedKnown {
			t.Fatal("preflight mutated the original context error")
		}
	}
	for _, known := range []bool{false, true} {
		zcodeTestRPC(t, func(request plugin.AccountRequest) (plugin.AccountResult, error) {
			if request.Operation == "context" {
				return zcodeTestData(t, c, nil), nil
			}
			return plugin.AccountResult{}, &plugin.AccountError{Kind: "account_scope_changed", Dispatched: known, DispatchedKnown: known}
		})
		ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
		_, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "billing.claim", map[string]string{"plan_id": "gift"})
		var accountErr *plugin.AccountError
		if !errors.As(err, &accountErr) || accountErr.Dispatched != known || accountErr.DispatchedKnown != known {
			t.Fatalf("post-operation dispatch state changed: known=%v err=%v", known, err)
		}
	}
}

func TestZCodeExecutionListAndScopedStatusHideJournalFields(t *testing.T) {
	c := zcodeTestContext(t)
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode": {"type": "oauth", "accountId": "alice", "refresh": "secret"},
	})
	executions := []zcodeExecution{
		{Lane: "highspeed", TaskID: "a-known", Scope: c.Scope, Generation: c.Generation, RawModel: "GLM-5.3", CardID: "card", State: "active"},
		{Lane: "offpeak", TaskID: "b-old", Scope: c.Scope, Generation: "old-generation", RawModel: "GLM-5.3", TicketID: "ticket-old", State: "ready"},
		{Lane: "offpeak", TaskID: "c-unknown", Scope: c.Scope, Generation: c.Generation, RawModel: "GLM-5.3", TicketID: "ticket-unknown", Claim: "claim-secret", State: "unknown"},
	}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		j.DrawAfter = 1900000000000
		j.Executions = map[string]zcodeExecution{}
		for _, e := range executions {
			j.Executions[e.TaskID] = e
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	zcodeTestRPC(t, func(request plugin.AccountRequest) (plugin.AccountResult, error) {
		switch request.Operation {
		case "context":
			return zcodeTestData(t, c, nil), nil
		case "offpeak.status":
			return zcodeTestData(t, c, map[string]any{"tickets": []any{map[string]any{"ticket_id": "ticket-unknown", "task_id": "c-unknown", "state": "active"}}}), nil
		default:
			return plugin.AccountResult{}, errors.New("unexpected account operation")
		}
	})
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	list, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "execution.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	var projection struct {
		Executions []struct {
			TaskID             string `json:"taskId"`
			RequiresResolution bool   `json:"requiresResolution"`
		} `json:"executions"`
		NextDrawAt int64 `json:"nextDrawAt"`
	}
	if err := json.Unmarshal(list.Data, &projection); err != nil || len(projection.Executions) != 3 || projection.NextDrawAt != 1900000000000 {
		t.Fatalf("list=%s err=%v", list.Data, err)
	}
	for i, want := range []struct {
		id         string
		unresolved bool
	}{{"a-known", false}, {"b-old", true}, {"c-unknown", true}} {
		got := projection.Executions[i]
		if got.TaskID != want.id || got.RequiresResolution != want.unresolved {
			t.Errorf("list row %d=%+v", i, got)
		}
	}
	assertPrivate := func(data []byte) {
		t.Helper()
		for _, field := range []string{`"scope"`, `"generation"`, `"claim"`, "claim-secret", "old-generation"} {
			if strings.Contains(string(data), field) {
				t.Errorf("private field %q in %s", field, data)
			}
		}
	}
	assertPrivate(list.Data)
	for _, task := range []string{"a-known", "b-old", "c-unknown"} {
		result, err := ZCodeAccountOperation(ctx, "zcode", "zcode", "execution.status", map[string]string{"taskId": task})
		if err != nil {
			t.Fatal(err)
		}
		assertPrivate(result.Data)
		if task == "b-old" && !strings.Contains(string(result.Data), `"requiresResolution":true`) || task == "c-unknown" && !strings.Contains(string(result.Data), `"observedState":"active"`) {
			t.Fatalf("status lost reconciliation information: %s", result.Data)
		}
	}
	unscoped, err := ZCodeAccountOperation(context.Background(), "zcode", "zcode", "execution.status", map[string]string{"taskId": "b-old"})
	if err != nil || !strings.Contains(string(unscoped.Data), `"requiresResolution":true`) {
		t.Fatalf("unscoped status lost reconciliation state: %s, %v", unscoped.Data, err)
	}
	assertPrivate(unscoped.Data)
}
