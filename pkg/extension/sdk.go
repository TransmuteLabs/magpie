package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/yetone/magpie/pkg/protocolext"
)

type Handler func(context.Context, Call, *Stream) (any, error)

type Module struct {
	Manifest Manifest
	Handlers map[string]Handler
	Inbounds []protocolext.Inbound
	Actions  map[string]Handler
	Start    StartFunc
}

func (module Module) Validate() error {
	_, _, err := module.prepare()
	return err
}

func (module Module) prepare() (map[string]Handler, *inboundModule, error) {
	if err := module.Manifest.Validate(); err != nil {
		return nil, nil, err
	}
	if module.Start != nil && module.Manifest.Contract.Version.Minor < 1 {
		return nil, nil, fmt.Errorf("module startup requires SDK API 1.1")
	}
	handlers := maps.Clone(module.Handlers)
	if handlers == nil {
		handlers = make(map[string]Handler)
	}
	if _, exists := handlers[MethodModuleCall]; exists {
		return nil, nil, fmt.Errorf("module.call is owned by the SDK; register Module.Actions")
	}
	actions := maps.Clone(module.Actions)
	declared := make(map[string]ActionDescriptor)
	for _, action := range module.Manifest.Actions {
		if actionOwner(action) == componentID(module.Manifest) {
			declared[action.ID] = action
			if actions[action.ID] == nil {
				return nil, nil, fmt.Errorf("module action requires handler %q", action.ID)
			}
		}
	}
	for id, handler := range actions {
		if _, found := declared[id]; !found || handler == nil {
			return nil, nil, fmt.Errorf("undeclared or invalid module action handler %q", id)
		}
	}
	if module.Manifest.Contract.Has(ModuleRPC) {
		handlers[MethodModuleCall] = func(ctx context.Context, call Call, stream *Stream) (any, error) {
			var request ModuleCallRequest
			if decodeParams(call.Params, &request) != nil || !json.Valid(request.Input) {
				return nil, &Error{Code: "invalid_params", Message: "module.call requires a declared action and JSON input"}
			}
			action, found := declared[request.Action]
			if !found {
				return nil, &Error{Code: "method_not_found", Message: "module action is not owned by this component"}
			}
			call.Action, call.Params = request.Action, slices.Clone(request.Input)
			stream.actionEffect = action.Effect
			return actions[request.Action](ctx, call, stream)
		}
	}
	var inbounds *inboundModule
	if len(module.Inbounds) > 0 {
		var err error
		inbounds, err = newInboundModule(module.Manifest, module.Inbounds)
		if err != nil {
			return nil, nil, err
		}
		for method, handler := range map[string]Handler{
			MethodInboundDecode: inbounds.decode, MethodInboundResponse: inbounds.response,
		} {
			if _, exists := handlers[method]; exists {
				return nil, nil, fmt.Errorf("inbound helpers conflict with handler %q", method)
			}
			handlers[method] = handler
		}
	}
	required := RequiredMethods(module.Manifest)
	for _, method := range required {
		if handlers[method] == nil {
			return nil, nil, fmt.Errorf("extension capability requires handler %q", method)
		}
	}
	for method, handler := range handlers {
		if handler == nil || !slices.Contains(required, method) {
			return nil, nil, fmt.Errorf("unnegotiated or invalid extension handler %q", method)
		}
	}
	snapshot := cloneManifest(module.Manifest)
	handlers[MethodDescribe] = func(context.Context, Call, *Stream) (any, error) {
		return snapshot.Public(), nil
	}
	return handlers, inbounds, nil
}
