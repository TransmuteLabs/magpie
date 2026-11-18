package extension

import (
	"context"
	"testing"
)

func TestLocalOwnerRequiresHostAuthority(t *testing.T) {
	type metadataKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(t.Context(), metadataKey{}, "local-owner"))
	defer cancel()
	if LocalOwner(nil) || LocalOwner(parent) {
		t.Fatal("unknown context acquired local owner authority")
	}
	owner := WithLocalOwner(parent, true)
	denied := WithLocalOwner(owner, false)
	if !LocalOwner(owner) || LocalOwner(denied) || denied.Value(metadataKey{}) != "local-owner" {
		t.Fatal("host authority did not override independently of client metadata")
	}
	cancel()
	if owner.Err() != context.Canceled || denied.Err() != context.Canceled {
		t.Fatal("caller policy lost its parent's cancellation")
	}
}
