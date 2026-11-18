package extension

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
)

func declaredBudgetPolicy() Manifest {
	m := testManifest(BudgetPolicy)
	m.Contract.Version.Minor = 13
	m.BudgetPolicies = []BudgetPolicyDescriptor{{ID: "extra"}}
	return m
}

func TestBudgetPolicyContract(t *testing.T) {
	m := declaredBudgetPolicy()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(RequiredMethods(m), []string{MethodBudgetReserve, MethodBudgetSpent}) {
		t.Fatal(RequiredMethods(m))
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Contract.Version.Minor = 12 },
		func(m *Manifest) { m.BudgetPolicies = nil },
		func(m *Manifest) { m.Component = &ComponentIdentity{} },
		func(m *Manifest) { m.BudgetPolicies[0].TimeoutMs = 1001 },
	} {
		invalid := m.Clone()
		change(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("admitted invalid budget declaration")
		}
	}
	raw := json.RawMessage(`{"policy":"extra","time":1,"scopes":[{"owner":"issuer","scoped":true,"tokens":"9223372036854775807","costUSD":0}]}`)
	req, err := DecodeBudgetRequest(m, MethodBudgetSpent, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, response := range []string{
		`{"restrictions":[{"scope":2,"retrySeconds":5}]}`,
		`{"restrictions":[{"scope":1,"retrySeconds":3601}]}`,
		`{"restrictions":[{"scope":1,"retrySeconds":5}],"models":["private"]}`,
		`{"restrictions":null}`,
		`{"Restrictions":[]}`,
		`{"restrictions":[{"Scope":1,"retrySeconds":5}]}`,
		`{"restrictions":[{"scope":1,"RetrySeconds":5}]}`,
	} {
		if _, err := DecodeBudgetResult(MethodBudgetSpent, req, json.RawMessage(response)); err == nil {
			t.Fatal("invalid budget result was allowed", response)
		}
	}
	for _, request := range []string{
		`{"Policy":"extra","time":1,"scopes":[{"owner":"issuer","scoped":true,"tokens":"10"}]}`,
		`{"policy":"extra","time":1,"scopes":[{"owner":"issuer","Scoped":true,"tokens":"10"}]}`,
		`{"policy":"extra","time":1,"scopes":[{"owner":"issuer","scoped":true,"tokens":"9223372036854775808"}]}`,
	} {
		if _, err := DecodeBudgetRequest(m, MethodBudgetSpent, json.RawMessage(request)); err == nil {
			t.Fatal("invalid budget request was allowed", request)
		}
	}
	if _, err := DecodeBudgetRequest(m, MethodBudgetSpent, json.RawMessage(`{"policy":"extra","time":1,"scopes":[],"body":"private"}`)); err == nil {
		t.Fatal("budget method accepted request body")
	}
}

func TestBudgetPolicyRPCIsUnaryAndHasNoHostAuthority(t *testing.T) {
	for _, mode := range []string{"normal", "auth", "scope", "event", "callback", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			m := declaredBudgetPolicy()
			var calls atomic.Int32
			handler := func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				calls.Add(1)
				switch mode {
				case "event":
					_ = stream.Emit(ctx, "accepted", struct{}{})
				case "callback":
					if err := stream.HostCall(ctx, MethodStateCAS, nil, nil); err == nil {
						t.Error("host callback was admitted")
					}
				case "invalid":
					return BudgetPolicyResult{Restrictions: []BudgetRestriction{{Scope: 2, RetrySeconds: 30}}}, nil
				}
				return BudgetPolicyResult{Restrictions: []BudgetRestriction{}}, nil
			}
			peer := startPeer(t, Module{Manifest: m, Handlers: map[string]Handler{MethodBudgetReserve: handler, MethodBudgetSpent: handler}})
			configurePeer(t, peer, m, "0")
			frame := callFrame("reserve", MethodBudgetReserve, BudgetPolicyRequest{
				Policy: "extra", Time: 1, BodyBytes: new(int64(10)), Scopes: []BudgetScope{{Owner: "key", Tokens: "20"}},
			})
			if mode == "auth" {
				frame.Auth = json.RawMessage(`{"credential":"private"}`)
			}
			if mode == "scope" {
				frame.Scope.Session = "private"
			}
			peer.send(frame)
			got := peer.read()
			if got.Kind != KindResult || (got.Error == nil) != (mode == "normal" || mode == "callback") {
				t.Fatalf("%s: %+v", mode, got)
			}
			wantCalls := 1
			if mode == "auth" || mode == "scope" {
				wantCalls = 0
			}
			if calls.Load() != int32(wantCalls) {
				t.Fatal("unexpected budget worker call count", calls.Load())
			}
		})
	}
}
