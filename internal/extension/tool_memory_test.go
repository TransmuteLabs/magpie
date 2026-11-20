package extension

import (
	"context"
	"errors"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestToolMemoryDiscoveryRespectsCancellation(t *testing.T) {
	manager := NewManager(t.TempDir())
	root, err := manager.catalog.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	unlock, err := catalogLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	lease := &Lease{manager: manager, worker: &managedWorker{}}
	ctx := WithToolMemorySearch(WithLocalOwner(t.Context(), true), func(context.Context, sdk.MemorySearchRequest) (sdk.MemorySearchResult, error) {
		t.Error("canceled grant read Memory")
		return sdk.MemorySearchResult{}, nil
	})
	ctx = grantToolMemory(ctx, lease, sdk.ToolDescriptor{Exposure: "local-owner", MemorySearch: true})
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.searchToolMemory(ctx, []byte(`{"query":"fixture"}`), hostAuthority{method: sdk.MethodToolCall})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled recall returned %v", err)
		}
	case <-time.After(2 * time.Second):
		unlock()
		<-done
		t.Fatal("canceled recall waited for the catalog lock")
	}
}
