package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type SearchBinding = contributionBinding[sdk.SearchSourceDescriptor]

func (m *Manager) SearchSources() []SearchBinding {
	return registeredContributions(context.Background(), m, "", sdk.Manifest.DeclaredSearchSources)
}

func (m *Manager) SearchSource(ctx context.Context, name string) (SearchBinding, bool) {
	module, id, found := contributionName(name)
	if !found {
		return SearchBinding{}, false
	}
	for _, binding := range registeredContributions(ctx, m, module, sdk.Manifest.DeclaredSearchSources) {
		if binding.Descriptor.ID == id {
			return binding, true
		}
	}
	return SearchBinding{}, false
}

func (m *Manager) checkSearch(ctx context.Context, binding SearchBinding, scope sdk.Scope) error {
	if err := checkContribution(ctx, m, binding, sdk.Manifest.DeclaredSearchSources); err != nil {
		return err
	}
	if scope.Plugin != binding.Module || scope.Provider != binding.Descriptor.Provider ||
		scope.Operation != "" || scope.Session != "" ||
		scope.Provider == "" && (scope.Account != "" || scope.Generation != 0) ||
		scope.Provider != "" && (scope.Account == "" || scope.Generation == 0) {
		return errors.New("extension search requires its declared account authority")
	}
	if _, err := m.authForContext(ctx, scope); err != nil {
		return err
	}
	return ctx.Err()
}

func (m *Manager) RunSearch(ctx context.Context, binding SearchBinding, scope sdk.Scope, request sdk.SearchRequest) (sdk.SearchResult, error) {
	if request.Source != binding.Descriptor.ID {
		return sdk.SearchResult{}, errors.New("extension search request does not name its source")
	}
	if err := request.Validate(); err != nil {
		return sdk.SearchResult{}, err
	}
	if err := m.checkSearch(ctx, binding, scope); err != nil {
		return sdk.SearchResult{}, err
	}
	ctx = withContributionAdmission(ctx, func(admission context.Context) error {
		return m.checkSearch(admission, binding, scope)
	})
	lease, err := binding.acquire(ctx, m)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return sdk.SearchResult{}, err
		}
		return sdk.SearchResult{}, errors.New("extension search source is unavailable")
	}
	defer lease.Close()
	if err := m.checkSearch(ctx, binding, scope); err != nil {
		return sdk.SearchResult{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return sdk.SearchResult{}, err
	}
	output, err := lease.Call(ctx, sdk.MethodModuleCall, scope, sdk.ModuleCallRequest{
		Action: binding.Descriptor.Action, Input: input,
	}, func(Event) error {
		return errors.New("extension search must return one unary result")
	})
	if err := ctx.Err(); err != nil {
		return sdk.SearchResult{}, err
	}
	if errors.Is(err, errContributionRevoked) || errors.Is(err, errContributionConfig) {
		return sdk.SearchResult{}, err
	}
	if err != nil || len(output) > sdk.MaxSearchBytes {
		return sdk.SearchResult{}, errors.New("extension search failed to return a bounded result")
	}
	var result sdk.SearchResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Validate(request.Count) != nil {
		return sdk.SearchResult{}, errors.New("extension search returned an invalid result")
	}
	if err := m.checkSearch(ctx, binding, scope); err != nil {
		return sdk.SearchResult{}, err
	}
	return result, nil
}
