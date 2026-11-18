package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type RoutingBinding struct {
	Module     string
	Revision   string
	Descriptor sdk.RoutingStrategyDescriptor
}

func (m *Manager) RoutingStrategies() []RoutingBinding {
	entries, err := m.List()
	if err != nil || m.isClosed() {
		return nil
	}
	out := []RoutingBinding{}
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.Contract.Version.Minor < 4 || !entry.Manifest.Contract.Has(sdk.ModuleRPC) {
			continue
		}
		for _, descriptor := range entry.Manifest.RoutingStrategies {
			out = append(out, RoutingBinding{Module: entry.Manifest.ID, Revision: entry.Revision, Descriptor: descriptor})
		}
	}
	return out
}

func (m *Manager) routingEntry(name string) (Installed, sdk.RoutingStrategyDescriptor, bool) {
	module, id, found := strings.Cut(name, "/")
	if !found || !sdk.ValidID(module) || !sdk.ValidID(id) || m.isClosed() {
		return Installed{}, sdk.RoutingStrategyDescriptor{}, false
	}
	entries, err := m.List()
	if err != nil {
		return Installed{}, sdk.RoutingStrategyDescriptor{}, false
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.ID != module || entry.Manifest.Contract.Version.Minor < 4 ||
			!entry.Manifest.Contract.Has(sdk.ModuleRPC) || entry.Manifest.Component != nil {
			continue
		}
		for _, descriptor := range entry.Manifest.RoutingStrategies {
			action, declared := entry.Manifest.ResolveAction(descriptor.Action)
			if descriptor.ID == id && declared && action.Effect == "read" {
				return entry, descriptor, true
			}
		}
	}
	return Installed{}, sdk.RoutingStrategyDescriptor{}, false
}

func (m *Manager) RoutingStrategy(name string) (RoutingBinding, bool) {
	entry, descriptor, found := m.routingEntry(name)
	if !found {
		return RoutingBinding{}, false
	}
	return RoutingBinding{Module: entry.Manifest.ID, Revision: entry.Revision, Descriptor: descriptor}, true
}

func (m *Manager) OrderRouting(parent context.Context, binding RoutingBinding, request sdk.RoutingRequest) (sdk.RoutingResult, error) {
	descriptor := binding.Descriptor
	if request.Strategy != descriptor.ID {
		return sdk.RoutingResult{}, errors.New("routing request does not name its descriptor")
	}
	if err := request.Validate(); err != nil {
		return sdk.RoutingResult{}, err
	}
	input, err := json.Marshal(request)
	if err != nil || len(input) > sdk.MaxRoutingBytes {
		return sdk.RoutingResult{}, errors.New("routing request exceeds its byte budget")
	}
	timeout := descriptor.TimeoutMs
	if timeout == 0 {
		timeout = sdk.RoutingTimeoutMs
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	name := binding.Module + "/" + descriptor.ID
	current, selected, available := m.routingEntry(name)
	if !available || current.Revision != binding.Revision || !reflect.DeepEqual(selected, descriptor) {
		return sdk.RoutingResult{}, errors.New("routing strategy changed before dispatch")
	}
	config, err := m.ModuleConfig(ctx, binding.Module)
	if err != nil {
		return sdk.RoutingResult{}, err
	}
	lease, err := m.AcquireRevision(ctx, binding.Module, binding.Revision, config.Version)
	if err != nil {
		return sdk.RoutingResult{}, err
	}
	defer lease.Close()
	if !reflect.DeepEqual(lease.Manifest().RoutingStrategies, current.Manifest.Public().RoutingStrategies) {
		return sdk.RoutingResult{}, errors.New("routing strategy leased descriptor changed")
	}
	output, err := lease.Call(ctx, sdk.MethodModuleCall, sdk.Scope{Plugin: binding.Module},
		sdk.ModuleCallRequest{Action: descriptor.Action, Input: input}, func(Event) error {
			return errors.New("routing strategy must return one unary order")
		})
	if err != nil || len(output) > sdk.MaxRoutingBytes {
		return sdk.RoutingResult{}, errors.New("routing strategy failed to return a bounded order")
	}
	var result sdk.RoutingResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Validate(len(request.Seats)) != nil {
		return sdk.RoutingResult{}, errors.New("routing strategy returned an invalid permutation")
	}
	current, selected, available = m.routingEntry(name)
	if !available || current.Revision != binding.Revision || !reflect.DeepEqual(selected, descriptor) {
		return sdk.RoutingResult{}, errors.New("routing strategy was revoked during ordering")
	}
	final, err := m.ModuleConfig(ctx, binding.Module)
	if err != nil || final.Version != lease.ConfigVersion() || ctx.Err() != nil {
		return sdk.RoutingResult{}, errors.New("routing strategy configuration changed or expired")
	}
	return sdk.RoutingResult{Order: append([]int(nil), result.Order...)}, nil
}
