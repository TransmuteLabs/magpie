package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAccountReplaceHostCallPreservesSelectedIdentityAfterAcceptedAck(t *testing.T) {
	manifest := testManifest(ToolProvider, AccountAuth)
	manifest.Providers = []ProviderDescriptor{providerDescriptor()}
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, OutboundExecutor)
	manifest.Tools = []ToolDescriptor{{Name: "reset", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	handlers := map[string]Handler{
		MethodAuthStart: testNoop, MethodAuthFinish: testNoop, MethodAuthRefresh: testNoop, MethodAuthLogout: testNoop,
		MethodProviderExecute: testNoop,
		MethodToolCall: func(ctx context.Context, call Call, stream *Stream) (any, error) {
			if err := stream.Emit(ctx, "accepted", AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			var result AccountReplaceResult
			err := stream.HostCall(ctx, MethodAccountReplace, AccountReplaceRequest{Credentials: json.RawMessage(`{"token":"replacement-secret"}`)}, &result)
			return result, err
		},
	}
	peer := startPeer(t, Module{Manifest: manifest, Handlers: handlers})
	call := callFrame("reset", MethodToolCall, ToolRequest{ID: "reset", Arguments: json.RawMessage(`{}`)})
	call.Scope.Provider, call.Scope.Account, call.Scope.Generation = "fixture", "selected", 7
	call.Scope.Operation = "reset-operation"
	call.Auth = json.RawMessage(`{"token":"old-secret"}`)
	peer.send(call)
	accepted := peer.read()
	if accepted.Kind != KindEvent || accepted.Event != "accepted" {
		t.Fatal("replacement started without acceptance", accepted)
	}
	peer.send(Frame{Kind: KindAck, ID: accepted.ID, Seq: accepted.Seq})
	callback := peer.read()
	var request AccountReplaceRequest
	if callback.Kind != KindHostCall || callback.Method != MethodAccountReplace || callback.Parent != call.ID ||
		callback.Scope != call.Scope || callback.Auth != nil || json.Unmarshal(callback.Params, &request) != nil ||
		string(request.Credentials) != `{"token":"replacement-secret"}` {
		t.Fatal("replacement escaped selected account authority", callback)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(callback.Params, &fields) != nil || len(fields) != 1 || fields["credentials"] == nil {
		t.Fatal("replacement request acquired arbitrary account selection", string(callback.Params))
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: json.RawMessage(`{"id":"selected","generation":8}`)})
	terminal := peer.read()
	var result AccountReplaceResult
	if terminal.Kind != KindResult || terminal.ID != call.ID || terminal.Error != nil ||
		json.Unmarshal(terminal.Result, &result) != nil || result.ID != "selected" || result.Generation != 8 ||
		strings.Contains(string(terminal.Result), "secret") {
		t.Fatal("replacement did not return a secret-free terminal identity", terminal)
	}
	if _, found := MethodCapability(MethodAccountReplace); found || len(FullContract().Capabilities) != 9 {
		t.Fatal("account replacement added a module capability")
	}
}
