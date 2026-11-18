package usage

import (
	"context"
	"net/http"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
)

func TestSDKRequestObserverQueueAndGenerationRemainBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serving := &extensionLedgerServing{ctx: ctx, queue: make(chan extensionLedgerDelivery, 1)}
	original := extensionhost.LedgerTarget{Module: "original", Revision: "v1", ConfigVersion: "1"}
	targets := []extensionhost.LedgerTarget{original, {Module: "overflow"}}
	serving.targets.Store(&targets)
	observer := &extensionRequestObserver{extensionLedgerObserver: extensionLedgerObserver{
		active: serving, request: true,
	}}
	request := Request{
		Agent: "syntheticprivateclient", Session: "syntheticprivatesession", Kind: "syntheticprivatekind",
		Header: http.Header{"Authorization": []string{"syntheticprivatecredential"}},
	}
	watch := observer.Begin(request)
	if watch == nil || observer.dropped.Load() != 1 {
		t.Fatalf("full request queue should drop exactly the competing start; watch=%v dropped=%d", watch != nil, observer.dropped.Load())
	}
	start := <-serving.queue
	if start.target != original || start.request.Phase != "start" || start.request.Agent != "other" || start.request.Kind != "" {
		t.Fatalf("unexpected start projection: %+v", start)
	}
	replacement := []extensionhost.LedgerTarget{{Module: "replacement", Revision: "v2", ConfigVersion: "2"}}
	serving.targets.Store(&replacement)
	watch.Attempt(Record{Time: time.Now(), Status: 429, Provider: "provider", Model: "invalid model label", Requested: "alias:model", BodyIn: "private", BodyOut: "private"})
	watch.Attempt(Record{Time: time.Now(), Status: 200})
	attempt := <-serving.queue
	if attempt.target != original || attempt.request.RequestID != start.request.RequestID || attempt.request.Attempt != 1 ||
		attempt.request.Status != 429 || attempt.request.Provider != "provider" || attempt.request.Model != "" || attempt.request.Requested != "alias:model" {
		t.Fatalf("unexpected attempt projection or generation: %+v", attempt)
	}
	if observer.dropped.Load() != 2 {
		t.Fatal("overloaded attempt should be dropped without retaining it")
	}
	watch.End(Record{Time: time.Now(), Status: 200})
	end := <-serving.queue
	if end.target != original || end.request.RequestID != start.request.RequestID || end.request.Phase != "end" || end.request.Status != 200 {
		t.Fatalf("unexpected end projection or generation: %+v", end)
	}
}
