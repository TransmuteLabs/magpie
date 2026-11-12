package extension

import (
	"context"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type callerIdentityKey struct{}
type nativeLeaseKey struct{}
type hostAuthorityKey struct{}
type hostAuthority struct {
	manifest sdk.Manifest
	method   string
}
type nativeLeaseRef struct {
	sync.Mutex
	lease *Lease
}

func WithCallerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerIdentityKey{}, id)
}

func CallerIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(callerIdentityKey{}).(string)
	return id
}

// WithInboundLease creates a host-owned slot. Decode fills it only after the
// authenticated request acquires a verified worker. Its generation then stays
// pinned for same-module provider/tool execution and response encoding.
func WithInboundLease(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeLeaseKey{}, &nativeLeaseRef{})
}

func pinInboundLease(ctx context.Context, lease *Lease) {
	if ref, ok := ctx.Value(nativeLeaseKey{}).(*nativeLeaseRef); ok {
		ref.Lock()
		if ref.lease == nil {
			ref.lease = lease
		}
		ref.Unlock()
	}
}

func inboundLeaseFrom(ctx context.Context) *Lease {
	if ref, ok := ctx.Value(nativeLeaseKey{}).(*nativeLeaseRef); ok {
		ref.Lock()
		defer ref.Unlock()
		return ref.lease
	}
	return nil
}
