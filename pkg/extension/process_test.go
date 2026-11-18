package extension

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestOwnedProcessRequestValidationKeepsAuthorityOutOfPayloads(t *testing.T) {
	for _, test := range []struct {
		method string
		params string
	}{
		{MethodProcessOpen, `{"recipeId":"native","executable":"/bin/sh"}`},
		{MethodProcessOpen, `{"recipeId":"native","env":{"KEY":"secret"}}`},
		{MethodProcessOpen, `{"recipeId":"native","account":"foreign"}`},
		{MethodProcessRead, `{"handle":"owner","pipe":"combined","seq":"1","maxBytes":64}`},
		{MethodProcessRead, `{"handle":"owner","pipe":"stdout","seq":"01","maxBytes":64}`},
		{MethodProcessRead, `{"handle":"owner","pipe":"stdout","seq":"1","maxBytes":65537}`},
		{MethodProcessRead, `{"handle":"owner","pipe":"stdout","seq":"1","seq":"2","maxBytes":64}`},
		{MethodProcessWrite, `{"handle":"owner","writeId":"write-1","seq":"0","bytes":"YQ=="}`},
		{MethodProcessClose, `{"handle":"owner","pid":123}`},
	} {
		if _, err := ProcessHostLane(test.method, json.RawMessage(test.params)); err == nil {
			t.Fatalf("process callback accepted invalid authority or bounds: %s %s", test.method, test.params)
		}
	}
	recipe := AgentProcessRecipe{ID: "native", Agent: "fixture", Argv: []string{"app-server", "--stdio", `'; $(touch forbidden)`}}
	if err := ValidateAgentProcessRecipe(recipe); err != nil {
		t.Fatal("literal arguments must retain their exact bytes", err)
	}
	recipe.Argv = []string{strings.Repeat("a", MaxProcessArgvBytes)}
	if ValidateAgentProcessRecipe(recipe) == nil {
		t.Fatal("process recipe exceeded its aggregate argument byte bound")
	}
}

func TestOwnedProcessCallbacksMakeProgressWhileOutputReadsWait(t *testing.T) {
	callbacks := []struct {
		method string
		params string
	}{
		{"process.read", `{"handle":"opaque-handle","pipe":"stdout","seq":"1","maxBytes":64}`},
		{"process.read", `{"handle":"opaque-handle","pipe":"stderr","seq":"1","maxBytes":64}`},
		{"process.write", `{"handle":"opaque-handle","writeId":"write-1","seq":"1","bytes":"cmVwbHkK"}`},
		{"process.close", `{"handle":"opaque-handle"}`},
	}
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		results := make(chan error, len(callbacks))
		var joined sync.WaitGroup
		for _, callback := range callbacks {
			joined.Add(1)
			go func() {
				defer joined.Done()
				results <- stream.HostCall(ctx, callback.method, json.RawMessage(callback.params), nil)
			}()
		}
		joined.Wait()
		close(results)
		for err := range results {
			if err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
	}))
	peer.send(callFrame("process-owner", MethodOperationStatus, nil))
	pending := make([]Frame, 0, len(callbacks))
	for range callbacks {
		frame := peer.read()
		if frame.Kind != KindHostCall || frame.Parent != "process-owner" {
			t.Fatalf("duplex callbacks did not progress independently while output reads waited: %+v", frame)
		}
		pending = append(pending, frame)
	}
	for _, frame := range pending {
		peer.send(Frame{Kind: KindHostResult, ID: frame.ID, Parent: frame.Parent, Result: json.RawMessage(`{}`)})
	}
	if result := peer.read(); result.ID != "process-owner" || result.Kind != KindResult || result.Error != nil {
		t.Fatalf("duplex parent did not join its callbacks: %+v", result)
	}
}

func TestOwnedProcessCancellationJoinsEveryPipeLane(t *testing.T) {
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		var joined sync.WaitGroup
		for _, pipe := range []string{"stdout", "stderr"} {
			joined.Add(1)
			go func() {
				defer joined.Done()
				_ = stream.HostCall(ctx, "process.read", map[string]any{
					"handle": "opaque-handle", "pipe": pipe, "seq": "1", "maxBytes": 64,
				}, nil)
			}()
		}
		joined.Wait()
		return struct{}{}, ctx.Err()
	}))
	peer.send(callFrame("process-owner", MethodOperationStatus, nil))
	for range 2 {
		if frame := peer.read(); frame.Kind != KindHostCall {
			t.Fatalf("both output lanes must be pending before cancellation: %+v", frame)
		}
	}
	peer.send(Frame{Kind: KindCancel, ID: "process-owner"})
	if result := peer.read(); result.Kind != KindResult || result.ID != "process-owner" ||
		result.Error == nil || result.Error.Code != "canceled" {
		t.Fatalf("parent cancellation did not join every pipe lane: %+v", result)
	}
}
