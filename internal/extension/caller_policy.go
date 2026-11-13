package extension

import "context"

type localOwnerKey struct{}

// WithLocalOwner records host-derived caller authority. Set it only at a
// trusted host entry point, never from module arguments or client metadata.
func WithLocalOwner(ctx context.Context, owner bool) context.Context {
	return context.WithValue(ctx, localOwnerKey{}, owner)
}

// LocalOwner is false when the host has not established caller ownership.
func LocalOwner(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	owner, _ := ctx.Value(localOwnerKey{}).(bool)
	return owner
}
