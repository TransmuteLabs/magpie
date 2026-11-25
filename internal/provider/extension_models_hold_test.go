package provider

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func TestHoldExtensionModelsSharesNonEmptyReadAndFailsClosed(t *testing.T) {
	var reads atomic.Int32
	var returnEmpty atomic.Bool
	binding := &extensionAccount{}
	account := &Account{
		Agent:      "ext-hold-test",
		User:       "acct",
		extension:  binding,
		magpieList: true,
		listedOnly: true,
	}
	account.extensionList = func(ctx context.Context) []catalog.Model {
		if ctx.Err() != nil {
			return nil
		}
		reads.Add(1)
		if returnEmpty.Load() {
			return nil
		}
		return []catalog.Model{{ID: "m1", Name: "Model 1", Price: &catalog.Price{Input: 1.5, Output: 2.5}}}
	}
	account.models = func() []catalog.Model { return account.extensionList(context.Background()) }
	base := Provider{ID: "ext-hold-test", Name: "Hold Test", Chat: "extension://ext-hold-test/v1", Account: account}

	returnEmpty.Store(true)
	held := HoldExtensionModels(t.Context(), []Provider{base})
	if got := held[0].Account.listedModels(t.Context()); len(got) != 0 {
		t.Fatalf("empty read = %v, want empty", got)
	}
	if reads.Load() != 1 {
		t.Fatalf("reads after empty = %d, want 1", reads.Load())
	}

	returnEmpty.Store(false)
	first := held[0].Account.listedModels(t.Context())
	if len(first) != 1 || first[0].ID != "m1" {
		t.Fatalf("first populated read = %v, want [m1]", first)
	}
	first[0].ID = "mutated"
	if reads.Load() != 2 {
		t.Fatalf("reads after first non-empty = %d, want 2", reads.Load())
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := held[0].Account.listedModels(t.Context())
			if len(got) != 1 || got[0].ID != "m1" {
				t.Errorf("concurrent read = %v, want [m1]", got)
			}
			if pr, ok := held[0].extensionListPrice("m1"); !ok || pr.Input != 1.5 {
				t.Errorf("extensionListPrice = (%+v, %v), want Input=1.5", pr, ok)
			}
		}()
	}
	wg.Wait()
	if reads.Load() != 2 {
		t.Fatalf("reads after cached fan-out = %d, want 2", reads.Load())
	}

	cancelCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := held[0].Account.listedModels(cancelCtx); got != nil {
		t.Fatalf("canceled callCtx read = %v, want nil", got)
	}
	heldCanceled := HoldExtensionModels(cancelCtx, []Provider{base})
	if got := heldCanceled[0].Account.listedModels(t.Context()); got != nil {
		t.Fatalf("canceled parent ctx read = %v, want nil", got)
	}
	if reads.Load() != 2 {
		t.Fatalf("canceled reads triggered loader: %d, want 2", reads.Load())
	}
}
