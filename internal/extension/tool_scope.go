package extension

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// checkLeasedTool is the final authority check. A discovery-time catalog
// check cannot authorize a replacement worker with different exposure.
func checkLeasedTool(ctx context.Context, manifest sdk.Manifest, params any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return errors.New("invalid extension tool request")
	}
	var request struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &request) != nil || request.ID == "" {
		return errors.New("extension tool request requires its declared name")
	}
	for _, tool := range manifest.Tools {
		if tool.Name != request.ID {
			continue
		}
		if tool.Exposure == "local-owner" && !LocalOwner(ctx) {
			return errors.New("this extension tool is available to this computer's owner alone")
		}
		return nil
	}
	return errors.New("extension tool is outside the executing generation's declaration")
}

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
