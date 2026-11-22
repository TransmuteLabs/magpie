package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// checkLeasedTool is the final authority check. A discovery-time catalog
// check cannot authorize a replacement worker with different exposure.
func checkLeasedTool(ctx context.Context, lease *Lease, params any) (context.Context, error) {
	if selected, ok := ctx.Value(toolAccountCheckKey{}).(ToolAccountSelection); ok {
		if lease.manager.Namespace() != selected.namespace || !selected.matches(lease.worker.entry) ||
			!sameModuleConfig(lease.worker.config, selected.config) {
			return ctx, errors.New("extension tool account selection belongs to another runtime generation")
		}
	}
	manifest := lease.worker.entry.Manifest
	body, err := json.Marshal(params)
	if err != nil {
		return ctx, errors.New("invalid extension tool request")
	}
	var request struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &request) != nil || request.ID == "" {
		return ctx, errors.New("extension tool request requires its declared name")
	}
	for _, tool := range manifest.Tools {
		if tool.Name != request.ID {
			continue
		}
		if tool.Exposure == "local-owner" && !LocalOwner(ctx) {
			return ctx, errors.New("this extension tool is available to this computer's owner alone")
		}
		return grantToolMemory(ctx, lease, tool), nil
	}
	return ctx, errors.New("extension tool is outside the executing generation's declaration")
}

type toolScopeKey struct{}

type toolAccountCheckKey struct{}

type ToolAccountSelection struct {
	namespace        string
	revision         string
	directory        string
	enableGeneration string
	config           sdk.ConfigSnapshot
}

func NewToolAccountSelection(namespace string, entry Installed, config sdk.ConfigSnapshot) (ToolAccountSelection, error) {
	values, err := effectiveModuleConfig(entry.Manifest, config.Values)
	if err != nil {
		return ToolAccountSelection{}, err
	}
	if len(values) == 0 {
		return ToolAccountSelection{}, errors.New("extension tool configuration is unavailable")
	}
	config.Values = bytes.Clone(values)
	return ToolAccountSelection{
		namespace: namespace, revision: entry.Revision, directory: entry.Directory,
		enableGeneration: entry.EnableGeneration, config: config,
	}, nil
}

func (selected ToolAccountSelection) matches(entry Installed) bool {
	return selected.revision == entry.Revision && selected.directory == entry.Directory &&
		selected.enableGeneration == entry.EnableGeneration
}

func (selected ToolAccountSelection) CheckConfig(entry Installed, config sdk.ConfigSnapshot) error {
	if !selected.matches(entry) {
		return errors.New("extension tool package or enablement changed")
	}
	if config.Version != selected.config.Version {
		return errors.New("extension tool configuration changed")
	}
	if bytes.Equal(config.Values, selected.config.Values) {
		return nil
	}
	values, err := effectiveModuleConfig(entry.Manifest, config.Values)
	config.Values = values
	if err != nil || !sameModuleConfig(config, selected.config) {
		return errors.New("extension tool configuration changed")
	}
	return nil
}

// WithToolAccountCheck pins the prepared account's runtime and retains its
// policy check through dispatch, acceptance and host callbacks.
func WithToolAccountCheck(ctx context.Context, selected ToolAccountSelection, check func(context.Context) error) context.Context {
	previous, _ := ctx.Value(windowAdmissionKey{}).(func(context.Context) error)
	ctx = context.WithValue(ctx, windowAdmissionKey{}, func(current context.Context) error {
		if previous != nil {
			if err := previous(current); err != nil {
				return err
			}
		}
		return check(current)
	})
	return context.WithValue(ctx, toolAccountCheckKey{}, selected)
}

// WithToolScope carries authority resolved by the host for one tool call.
func WithToolScope(ctx context.Context, scope sdk.Scope) context.Context {
	return context.WithValue(ctx, toolScopeKey{}, scope)
}

// ToolScopeFrom returns host-derived authority, or an unbound scope.
func ToolScopeFrom(ctx context.Context) sdk.Scope {
	scope, _ := ctx.Value(toolScopeKey{}).(sdk.Scope)
	return scope
}
