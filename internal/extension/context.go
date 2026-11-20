package extension

import (
	"context"
	"sync"
	"sync/atomic"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type callerIdentityKey struct{}
type nativeLeaseKey struct{}
type hostAuthorityKey struct{}
type hostAuthority struct {
	manifest   sdk.Manifest
	method     string
	invocation *invocationAuthority
}
type invocationAuthority struct {
	scope       sdk.Scope
	replacement atomic.Uint64
}
type publicationAuthorityKey struct{}
type publicationAuthority struct {
	namespace  string
	invocation *invocationAuthority
	active     atomic.Bool
}
type nativeLeaseRef struct {
	sync.Mutex
	lease *Lease
}

func AccountPublicationScope(ctx context.Context, namespace string, selected sdk.Scope) sdk.Scope {
	proof, ok := ctx.Value(publicationAuthorityKey{}).(*publicationAuthority)
	authority, authorized := ctx.Value(hostAuthorityKey{}).(hostAuthority)
	if !ok || !authorized || !proof.active.Load() || proof.namespace != namespace ||
		proof.invocation == nil || authority.invocation != proof.invocation || ctx.Err() != nil {
		return selected
	}
	original := proof.invocation.scope
	if selected.Plugin != original.Plugin || selected.Provider != original.Provider ||
		selected.Account != original.Account || selected.Generation != original.Generation ||
		selected.Session != original.Session || selected.Operation != "" && selected.Operation != original.Operation ||
		selected.Attempt != 0 && selected.Attempt != original.Attempt {
		return selected
	}
	if generation := proof.invocation.replacement.Load(); generation != 0 {
		selected.Generation = generation
	}
	return selected
}

func (m *Manager) checkResultPublication(ctx context.Context, invocation *invocationAuthority) error {
	proof := &publicationAuthority{namespace: m.Namespace(), invocation: invocation}
	proof.active.Store(true)
	defer proof.active.Store(false)
	ctx = context.WithValue(ctx, publicationAuthorityKey{}, proof)
	if _, err := m.authForContext(ctx, AccountPublicationScope(ctx, proof.namespace, invocation.scope)); err != nil {
		return err
	}
	return checkWindowAdmission(ctx)
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
