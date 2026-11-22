package extension

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type callerIdentityKey struct{}
type callerIdentity struct {
	id       string
	describe func(context.Context) *sdk.ExecuteCaller
}
type nativeLeaseKey struct{}
type providerExecutionBindingKey struct{}
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
	lease   *Lease
	decoded bool
}

type ProviderExecutionBinding struct {
	Namespace        string
	PackageRevision  string
	ConfigVersion    string
	EnableGeneration string
	Scope            sdk.Scope
}

func WithProviderExecutionBinding(ctx context.Context, record func(ProviderExecutionBinding)) context.Context {
	return context.WithValue(ctx, providerExecutionBindingKey{}, record)
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
	return WithCaller(ctx, id, nil)
}

func WithCaller(ctx context.Context, id string, describe func(context.Context) *sdk.ExecuteCaller) context.Context {
	return context.WithValue(ctx, callerIdentityKey{}, callerIdentity{id: id, describe: describe})
}

func CallerIDFrom(ctx context.Context) string {
	caller, _ := ctx.Value(callerIdentityKey{}).(callerIdentity)
	return caller.id
}

func providerExecutionParams(ctx context.Context, minor int, params any) (json.RawMessage, error) {
	var projection *sdk.ExecuteCaller
	if caller, ok := ctx.Value(callerIdentityKey{}).(callerIdentity); ok && caller.describe != nil && minor >= sdk.ExecuteCallerAPIMinor {
		projection = caller.describe(ctx)
	}
	if request, ok := params.(sdk.ExecuteRequest); ok {
		request.Caller = projection
		return json.Marshal(request)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw, nil
	}
	changed := projection != nil
	for key := range fields {
		if strings.EqualFold(key, "caller") {
			delete(fields, key)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	if projection != nil {
		value, err := json.Marshal(projection)
		if err != nil {
			return nil, err
		}
		fields["caller"] = value
	}
	return json.Marshal(fields)
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

func markInboundLeaseDecoded(ctx context.Context, lease *Lease) {
	if ref, ok := ctx.Value(nativeLeaseKey{}).(*nativeLeaseRef); ok {
		ref.Lock()
		if ref.lease == lease {
			ref.decoded = true
		}
		ref.Unlock()
	}
}

func isDecodedInboundLease(ctx context.Context, lease *Lease) bool {
	if ref, ok := ctx.Value(nativeLeaseKey{}).(*nativeLeaseRef); ok {
		ref.Lock()
		defer ref.Unlock()
		return ref.decoded && ref.lease == lease
	}
	return false
}

func inboundLeaseFrom(ctx context.Context) *Lease {
	if ref, ok := ctx.Value(nativeLeaseKey{}).(*nativeLeaseRef); ok {
		ref.Lock()
		defer ref.Unlock()
		return ref.lease
	}
	return nil
}
