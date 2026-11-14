package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type SearchBinding struct {
	Namespace     string
	Module        string
	Revision      string
	ConfigVersion string
	Descriptor    sdk.SearchSourceDescriptor
}

func (m *Manager) SearchSources() []SearchBinding {
	entries, err := m.List()
	if err != nil || m.isClosed() {
		return nil
	}
	out := []SearchBinding{}
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.Contract.Version.Minor < 8 || !entry.Manifest.Contract.Has(sdk.ModuleRPC) {
			continue
		}
		config, err := m.ModuleConfig(context.Background(), entry.Manifest.ID)
		if err != nil {
			continue
		}
		for _, descriptor := range entry.Manifest.SearchSources {
			out = append(out, SearchBinding{Namespace: m.Namespace(), Module: entry.Manifest.ID,
				Revision: entry.Revision, ConfigVersion: config.Version, Descriptor: descriptor})
		}
	}
	return out
}

func (m *Manager) SearchSource(name string) (SearchBinding, bool) {
	module, id, found := strings.Cut(name, "/")
	if !found || !sdk.ValidID(module) || !sdk.ValidID(id) {
		return SearchBinding{}, false
	}
	for _, binding := range m.SearchSources() {
		if binding.Module == module && binding.Descriptor.ID == id {
			return binding, true
		}
	}
	return SearchBinding{}, false
}

func (m *Manager) checkSearch(ctx context.Context, binding SearchBinding, scope sdk.Scope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, found := m.SearchSource(binding.Module + "/" + binding.Descriptor.ID)
	if !found || binding.Namespace != m.Namespace() || !reflect.DeepEqual(current, binding) {
		return errors.New("extension search source or configuration changed")
	}
	if scope.Plugin != binding.Module || scope.Provider != binding.Descriptor.Provider ||
		scope.Operation != "" || scope.Session != "" ||
		scope.Provider == "" && (scope.Account != "" || scope.Generation != 0) ||
		scope.Provider != "" && (scope.Account == "" || scope.Generation == 0) {
		return errors.New("extension search requires its declared account authority")
	}
	if _, err := m.authFor(scope); err != nil {
		return err
	}
	return nil
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
	previous, _ := ctx.Value(windowAdmissionKey{}).(func(context.Context) error)
	ctx = context.WithValue(ctx, windowAdmissionKey{}, func(admission context.Context) error {
		if previous != nil {
			if err := previous(admission); err != nil {
				return err
			}
		}
		return m.checkSearch(admission, binding, scope)
	})
	lease, err := m.AcquireRevision(ctx, binding.Module, binding.Revision, binding.ConfigVersion)
	if err != nil {
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
