package extension

import (
	"context"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type toolScopeKey struct{}

// WithToolScope carries authority resolved by the host for one tool call.
func WithToolScope(ctx context.Context, scope sdk.Scope) context.Context {
	return context.WithValue(ctx, toolScopeKey{}, scope)
}

// ToolScopeFrom returns host-derived authority, or an unbound scope.
func ToolScopeFrom(ctx context.Context) sdk.Scope {
	scope, _ := ctx.Value(toolScopeKey{}).(sdk.Scope)
	return scope
}
