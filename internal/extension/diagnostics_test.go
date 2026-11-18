package extension

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModuleDiagnosticsReachTheExistingHostWithSelectedAuthority(t *testing.T) {
	manager, scope := usageManagerVersion(t, 7)
	var records []moduleDiagnostic
	manager.diagnosticLog = func(record moduleDiagnostic) { records = append(records, record) }
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil {
		t.Fatal(err)
	}
	var result sdk.DiagnosticResult
	err = manager.Invoke(t.Context(), scope.Plugin, sdk.MethodToolCall, scope, sdk.ToolRequest{ID: "tool",
		Arguments: json.RawMessage(`{"action":"diagnostic","value":{"level":"warn","code":"vendor_timeout","phase":"provider.execute","status":504,"millis":1200}}`)},
		&result, nil)
	if err != nil || !result.Recorded || len(records) != 1 {
		t.Fatalf("ordinary executable diagnostic callback failed: %+v %v %+v", result, err, records)
	}
	record := records[0]
	if record.Plugin != scope.Plugin || record.Component != sdk.PrimaryComponent || record.Provider != scope.Provider ||
		record.Account != scope.Account || record.Value.Code != "vendor_timeout" || record.Value.Status != 504 {
		t.Fatalf("host-derived diagnostic identity was lost: %+v", record)
	}
	after, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || !reflect.DeepEqual(after, account) {
		t.Fatal("observing a diagnostic changed selected account credentials", err)
	}
}

func TestModuleDiagnosticsRejectForgedStaleCanceledAndExcessCalls(t *testing.T) {
	manager, scope := usageManagerVersion(t, 7)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	authority := hostAuthority{manifest: lease.worker.entry.Manifest, method: sdk.MethodOperationStatus}
	ctx := context.WithValue(t.Context(), hostAuthorityKey{}, authority)
	recorded := 0
	manager.diagnosticLog = func(moduleDiagnostic) { recorded++ }
	raw := json.RawMessage(`{"level":"info","code":"request_started"}`)
	for range 50 {
		result, err := manager.hostCall(ctx, scope, sdk.MethodDiagnosticEmit, raw)
		if err != nil || !result.(sdk.DiagnosticResult).Recorded {
			t.Fatal("bounded diagnostic call was lost", result, err)
		}
	}
	result, err := manager.hostCall(ctx, scope, sdk.MethodDiagnosticEmit, raw)
	if err != nil || result.(sdk.DiagnosticResult).Recorded || recorded != 50 {
		t.Fatalf("diagnostics exceeded the module-wide log budget: %+v %v %d", result, err, recorded)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"level":"info","code":"request_started","plugin":"another-module"}`),
		json.RawMessage(`{"level":"error","code":"failed","message":"Authorization: Bearer private"}`),
		json.RawMessage(`{"level":"error","code":"failed","body":"private request body"}`),
	} {
		if _, err := manager.hostCall(ctx, scope, sdk.MethodDiagnosticEmit, invalid); err == nil {
			t.Fatal("diagnostic accepted caller identity or unstructured secrets", string(invalid))
		}
	}
	stale := scope
	stale.Generation++
	if _, err := manager.hostCall(ctx, stale, sdk.MethodDiagnosticEmit, raw); err == nil {
		t.Fatal("revoked account generation emitted a diagnostic")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := manager.hostCall(canceled, scope, sdk.MethodDiagnosticEmit, raw); err == nil {
		t.Fatal("canceled call emitted a diagnostic")
	}
	authority.manifest.Contract.Version.Minor = 6
	old := context.WithValue(ctx, hostAuthorityKey{}, authority)
	if _, err := manager.hostCall(old, scope, sdk.MethodDiagnosticEmit, raw); err == nil {
		t.Fatal("old contract emitted structured diagnostics")
	}
	if _, err := manager.hostCall(t.Context(), scope, sdk.MethodDiagnosticEmit, raw); err == nil {
		t.Fatal("unowned call emitted a diagnostic")
	}
	if recorded != 50 {
		t.Fatal("refused callbacks reached the log", recorded)
	}
}

func TestModuleDiagnosticNoiseLeavesRoomForAnotherAccountsFailure(t *testing.T) {
	manager, scope := usageManagerVersion(t, 7)
	failed := scope
	failed.Account = "another-account"
	storeTestAccount(t, manager.Store(), failed)
	var records []moduleDiagnostic
	manager.diagnosticLog = func(record moduleDiagnostic) { records = append(records, record) }
	emit := func(selected sdk.Scope, level string) bool {
		t.Helper()
		var result sdk.DiagnosticResult
		err := manager.Invoke(t.Context(), selected.Plugin, sdk.MethodToolCall, selected, sdk.ToolRequest{ID: "tool",
			Arguments: json.RawMessage(`{"action":"diagnostic","value":{"code":"fixture_request","level":"` + level + `"}}`)},
			&result, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.Recorded
	}
	for range 60 {
		emit(scope, "info")
	}
	for range 5 {
		if !emit(failed, "warn") {
			t.Fatal("successful requests exhausted the budget for another account's warning")
		}
	}
	if emit(failed, "warn") {
		t.Fatal("warnings consumed the entries reserved for errors")
	}
	last := records[len(records)-1]
	if last.Account != failed.Account || last.Value.Level != "warn" {
		t.Fatalf("wrong account warning recorded: %+v", last)
	}
	if !emit(failed, "error") {
		t.Fatal("routine diagnostics and warnings exhausted the budget for another account's error")
	}
	last = records[len(records)-1]
	if last.Account != failed.Account || last.Value.Level != "error" {
		t.Fatalf("wrong account diagnostic recorded: %+v", last)
	}
	for range 60 {
		emit(failed, "error")
	}
	if emit(failed, "error") || len(records) != 60 {
		t.Fatalf("diagnostics exceeded the total module budget: %d", len(records))
	}
}
