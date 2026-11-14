package extension

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func accountTasksManifest(t *testing.T) Manifest {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal([]byte(`{
		"schema":1,"id":"tasks.fixture","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":9},"capabilities":["outbound-executor","account-auth","module-rpc","usage-reporter"]},
		"providers":[{"id":"vendor","name":"Fixture","protocols":["chat"],"accountTasks":[{"id":"ping","action":"paid.ping"}]}],
		"actions":[{"id":"paid.ping","effect":"write","inputSchema":{"type":"object"}}]
	}`), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestAccountTasksRequireNegotiatedSelectedPaidWrite(t *testing.T) {
	manifest := accountTasksManifest(t)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	task, found := manifest.AccountTask("vendor", "ping")
	if !found || task.Action != "paid.ping" {
		t.Fatal("paid task descriptor was lost", task, found)
	}
	for _, change := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 8 },
		func(value *Manifest) {
			value.Contract.Capabilities = []Capability{OutboundExecutor, AccountAuth, ModuleRPC}
		},
		func(value *Manifest) { value.Actions[0].Effect = "read" },
		func(value *Manifest) { value.Providers[0].AccountTasks[0].Action = "missing" },
		func(value *Manifest) { value.Providers[0].AccountTasks = append(value.Providers[0].AccountTasks, task) },
	} {
		copy := manifest.Clone()
		change(&copy)
		if err := copy.Validate(); err == nil {
			t.Fatal("invalid paid task contract was admitted", copy)
		}
	}
	copy := manifest.Clone()
	copy.Providers[0].AccountTasks[0].Action = "other"
	if manifest.Providers[0].AccountTasks[0].Action != "paid.ping" {
		t.Fatal("public descriptor clone retained mutable task authority")
	}
}

func TestAccountTaskWireRejectsIdentityAndUnboundedPayload(t *testing.T) {
	for _, raw := range []string{
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":1794656400000}`,
		`{"task":"ping","key":"conversation","job":"opaque-template-2","dueAt":9007199254740991}`,
	} {
		if _, err := DecodeAccountTaskSchedule([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`null`,
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":-1}`,
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":9007199254740992}`,
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":1794656400000.5}`,
		`{"task":"ping","key":"","job":"opaque-template","dueAt":1794656400000}`,
		`{"task":"ping","key":"conversation","job":"","dueAt":1794656400000}`,
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":1794656400000,"input":{}}`,
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":1794656400000,"account":"other"}`,
		`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":1794656400000,"model":"other"}`,
	} {
		if _, err := DecodeAccountTaskSchedule([]byte(raw)); err == nil {
			t.Fatal("unbounded or arbitrary-identity task accepted", raw)
		}
	}
	manifest := accountTasksManifest(t)
	for _, raw := range []string{
		`{"outcome":"not-started"}`,
		`{"outcome":"uncertain"}`,
		`{"outcome":"completed","call":{"kind":"cache_keepalive","model":"m1","usage":{"inputTokens":3}}}`,
	} {
		if _, err := DecodeAccountTaskResult(manifest, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"outcome":"accepted"}`, `{"outcome":"not-started","account":"other"}`} {
		if _, err := DecodeAccountTaskResult(manifest, []byte(raw)); err == nil {
			t.Fatal("invalid terminal task receipt accepted", raw)
		}
	}
}

func TestAccountTaskUsageEventPreservesFactsAndRejectsCanceledStream(t *testing.T) {
	exerciseAccountTaskUsageCancellation(t, accountTasksManifest(t))
}

func exerciseAccountTaskUsageCancellation(t *testing.T, manifest Manifest) {
	t.Helper()
	manifest.ID = "fixture"
	handlers := map[string]Handler{}
	for _, method := range RequiredMethods(manifest) {
		if method != MethodModuleCall {
			handlers[method] = testNoop
		}
	}
	input, output, cached := int64(17), int64(2), int64(13)
	responseID := "native-response"
	measured := OwnCall{Kind: "cache_keepalive", Model: "m1",
		Usage: Usage{Facts: &ExecutionUsageFacts{ExecutionCounts: ExecutionCounts{
			InputTokens: &input, OutputTokens: &output, CachedTokens: &cached}, ResponseID: &responseID}}}
	afterCancel := make(chan error, 1)
	reported := make(chan struct{})
	actions := map[string]Handler{}
	for _, action := range manifest.Actions {
		actions[action.ID] = testNoop
	}
	actions["paid.ping"] = func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if err := stream.Emit(ctx, "accepted", AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
		if err := stream.ReportAccountTaskUsage(ctx, measured); err != nil {
			return nil, err
		}
		close(reported)
		<-ctx.Done()
		afterCancel <- stream.ReportAccountTaskUsage(context.Background(), measured)
		return AccountTaskResult{Outcome: "uncertain"}, nil
	}
	module := Module{Manifest: manifest, Handlers: handlers, Actions: actions}
	peer := startPeer(t, module)
	configurePeer(t, peer, manifest, "1")
	frame := callFrame("tick", MethodModuleCall, ModuleCallRequest{Action: "paid.ping",
		Input: json.RawMessage(`{"task":"ping","key":"conversation","job":"opaque-template","dueAt":1794656400000,"model":"m1"}`)})
	frame.Scope.Provider, frame.Scope.Account, frame.Scope.Generation = "vendor", "selected", 1
	frame.Scope.Operation, frame.Scope.Attempt = "account-task-selected", 1
	peer.send(frame)
	accepted := peer.read()
	if accepted.Kind != KindEvent || accepted.Event != "accepted" {
		t.Fatal("task lost its accepted receipt", accepted)
	}
	peer.send(Frame{Kind: KindAck, ID: accepted.ID, Seq: accepted.Seq})
	snapshot := peer.read()
	var actual OwnCall
	if snapshot.Kind != KindEvent || snapshot.Event != EventAccountTaskUsage ||
		json.Unmarshal(snapshot.Data, &actual) != nil || actual.Usage.Facts == nil ||
		actual.Usage.Facts.InputTokens == nil || *actual.Usage.Facts.InputTokens != input ||
		actual.Usage.Facts.ResponseID == nil || *actual.Usage.Facts.ResponseID != responseID {
		t.Fatal("task usage lost measured facts", snapshot)
	}
	peer.send(Frame{Kind: KindAck, ID: snapshot.ID, Seq: snapshot.Seq})
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not finish its acknowledged progress callback")
	}
	peer.send(Frame{Kind: KindCancel, ID: frame.ID})
	if terminal := peer.read(); terminal.Kind != KindResult || terminal.Error == nil {
		t.Fatal("canceled task omitted its terminal result", terminal)
	}
	select {
	case err := <-afterCancel:
		if err == nil {
			t.Fatal("canceled task regained event authority through a background context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled task handler did not finish")
	}
}
