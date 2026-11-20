package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type RoutingBinding = contributionBinding[sdk.RoutingStrategyDescriptor]

var errRoutingRevoked = errors.New("routing strategy was revoked during ordering")

func (m *Manager) RoutingStrategies() []RoutingBinding {
	return registeredContributions(context.Background(), m, "", sdk.Manifest.DeclaredRoutingStrategies)
}

func (m *Manager) RoutingStrategy(name string) (RoutingBinding, bool) {
	module, id, found := contributionName(name)
	if !found {
		return RoutingBinding{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sdk.MaxRoutingTimeoutMs*time.Millisecond)
	defer cancel()
	for _, binding := range registeredContributions(ctx, m, module, sdk.Manifest.DeclaredRoutingStrategies) {
		if binding.Descriptor.ID == id {
			return binding, true
		}
	}
	return RoutingBinding{}, false
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
	check := func(admission context.Context) error {
		err := checkContribution(admission, m, binding, sdk.Manifest.DeclaredRoutingStrategies)
		if errors.Is(err, errContributionRevoked) {
			return errRoutingRevoked
		}
		return err
	}
	if err := check(ctx); err != nil {
		return sdk.RoutingResult{}, err
	}
	ctx = withContributionAdmission(ctx, check)
	lease, err := binding.acquire(ctx, m)
	if err != nil {
		return sdk.RoutingResult{}, err
	}
	defer lease.Close()
	if err := ctx.Err(); err != nil {
		return sdk.RoutingResult{}, err
	}
	output, err := lease.Call(ctx, sdk.MethodModuleCall, sdk.Scope{Plugin: binding.Module},
		sdk.ModuleCallRequest{Action: descriptor.Action, Input: input}, func(Event) error {
			return errors.New("routing strategy must return one unary order")
		})
	if errors.Is(err, errRoutingRevoked) || errors.Is(err, errContributionConfig) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sdk.RoutingResult{}, err
	}
	if err != nil || len(output) > sdk.MaxRoutingBytes {
		return sdk.RoutingResult{}, errors.New("routing strategy failed to return a bounded order")
	}
	var result sdk.RoutingResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Validate(len(request.Seats)) != nil {
		return sdk.RoutingResult{}, errors.New("routing strategy returned an invalid permutation")
	}
	if err := check(ctx); err != nil {
		return sdk.RoutingResult{}, err
	}
	return sdk.RoutingResult{Order: append([]int(nil), result.Order...)}, nil
}
