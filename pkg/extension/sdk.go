package extension

import (
	"context"
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
}

func (module Module) Validate() error {
	_, _, err := module.prepare()
	return err
}

func (module Module) prepare() (map[string]Handler, *inboundModule, error) {
	if err := module.Manifest.Validate(); err != nil {
		return nil, nil, err
	}
	handlers := maps.Clone(module.Handlers)
	if handlers == nil {
		handlers = make(map[string]Handler)
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
