package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	GatewayMiddleware Capability = "gateway-middleware"

	MethodMiddlewareRequest  = "middleware.request"
	MethodMiddlewareAccount  = "middleware.account"
	MethodMiddlewareProvider = "middleware.provider"
	MethodMiddlewareEvent    = "middleware.event"
	MethodMiddlewareResponse = "middleware.response"
	MaxMiddlewareBodyBytes   = 1 << 20
)

type GatewayMiddlewareDescriptor struct {
	Request  bool     `json:"request,omitempty"`
	Account  bool     `json:"account,omitempty"`
	Provider bool     `json:"provider,omitempty"`
	Event    bool     `json:"event,omitempty"`
	Response bool     `json:"response,omitempty"`
	Events   []string `json:"events,omitempty"`
	Origins  []string `json:"origins,omitempty"`
}

type MiddlewareProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	Model   string `json:"model"`
}

type MiddlewareAccount struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Model    string `json:"model"`
}

type MiddlewareContext struct {
	GatewayContext
	Provider *MiddlewareProvider `json:"provider,omitempty"`
	Account  *MiddlewareAccount  `json:"account,omitempty"`
	Event    string              `json:"event,omitempty"`
	Status   int                 `json:"status,omitempty"`
}

type MiddlewareCall struct {
	Body    json.RawMessage   `json:"body,omitempty"`
	State   json.RawMessage   `json:"state,omitempty"`
	Context MiddlewareContext `json:"context"`
}

type MiddlewareRejection struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type MiddlewareResult struct {
	Body   json.RawMessage      `json:"body,omitempty"`
	State  json.RawMessage      `json:"state,omitempty"`
	Reject *MiddlewareRejection `json:"reject,omitempty"`
	Skip   bool                 `json:"skip,omitempty"`
	Drop   bool                 `json:"drop,omitempty"`
}

func IsMiddlewareMethod(method string) bool {
	return slices.Contains([]string{MethodMiddlewareRequest, MethodMiddlewareAccount,
		MethodMiddlewareProvider, MethodMiddlewareEvent, MethodMiddlewareResponse}, method)
}

func (descriptor GatewayMiddlewareDescriptor) Methods() []string {
	var methods []string
	for _, method := range []string{MethodMiddlewareRequest, MethodMiddlewareAccount,
		MethodMiddlewareProvider, MethodMiddlewareEvent, MethodMiddlewareResponse} {
		if descriptor.DeclaresMethod(method) {
			methods = append(methods, method)
		}
	}
	return methods
}

func (descriptor GatewayMiddlewareDescriptor) DeclaresMethod(method string) bool {
	switch method {
	case MethodMiddlewareRequest:
		return descriptor.Request
	case MethodMiddlewareAccount:
		return descriptor.Account
	case MethodMiddlewareProvider:
		return descriptor.Provider
	case MethodMiddlewareEvent:
		return descriptor.Event
	case MethodMiddlewareResponse:
		return descriptor.Response
	default:
		return false
	}
}

func (descriptor GatewayMiddlewareDescriptor) AllowsOrigin(origin string) bool {
	return (GatewayHookDescriptor{Origins: descriptor.Origins}).AllowsOrigin(origin)
}

func (manifest Manifest) validateMiddleware() error {
	descriptor := manifest.Middleware
	if descriptor == nil && !manifest.Contract.Has(GatewayMiddleware) {
		return nil
	}
	if descriptor == nil || !manifest.Contract.Has(GatewayMiddleware) ||
		manifest.Contract.Version.Minor < 8 || manifest.Component != nil || len(descriptor.Methods()) == 0 {
		return fmt.Errorf("gateway middleware requires root API 1.8, gateway-middleware and declared hooks")
	}
	if len(descriptor.Events) > 64 || len(descriptor.Events) > 0 && !descriptor.Event {
		return fmt.Errorf("middleware event filters require an event hook within limits")
	}
	seen := map[string]bool{}
	for _, name := range descriptor.Events {
		if !searchString(name, 128) || seen[name] {
			return fmt.Errorf("invalid or duplicate middleware event filter")
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	for _, origin := range descriptor.Origins {
		if !validGatewayOrigin(origin) || seen[origin] {
			return fmt.Errorf("invalid or duplicate middleware origin")
		}
		seen[origin] = true
	}
	return nil
}

func ValidateMiddlewareScope(scope Scope) error {
	if scope.Provider != "" || scope.Account != "" || scope.Generation != 0 ||
		scope.Operation != "" || scope.Attempt != 0 || scope.Session != "" {
		return &Error{Code: "permission_denied", Message: "middleware cannot borrow account, session or write authority"}
	}
	return nil
}

func DecodeMiddlewareCall(descriptor GatewayMiddlewareDescriptor, method string, scope Scope, raw json.RawMessage) (MiddlewareCall, error) {
	var call MiddlewareCall
	if err := ValidateMiddlewareScope(scope); err != nil {
		return call, err
	}
	if !descriptor.DeclaresMethod(method) || decodeParams(raw, &call) != nil {
		return call, &Error{Code: "invalid_params", Message: "invalid or undeclared middleware call"}
	}
	return call, validateMiddlewareCall(descriptor, method, call)
}

func ValidateMiddlewareCall(descriptor GatewayMiddlewareDescriptor, method string, scope Scope, call MiddlewareCall) error {
	if err := ValidateMiddlewareScope(scope); err != nil {
		return err
	}
	if !descriptor.DeclaresMethod(method) || !middlewareJSONWithin(call.Body, MaxMiddlewareBodyBytes) ||
		!middlewareJSONWithin(call.State, MaxGatewayStateBytes) {
		return &Error{Code: "invalid_params", Message: "invalid or undeclared middleware call"}
	}
	return validateMiddlewareCall(descriptor, method, call)
}

func middlewareJSONWithin(raw json.RawMessage, limit int) bool {
	if len(raw) > limit {
		return false
	}
	if len(raw) <= limit/6 || !bytes.ContainsAny(raw, "<>&\u2028\u2029") {
		return true
	}
	encoded, err := json.Marshal(raw)
	return err == nil && len(encoded) <= limit
}

func middlewareString(value string, limit int) bool {
	return searchString(value, limit) ||
		len(value) <= limit && !utf8.ValidString(value) && searchString(string([]rune(value)), limit)
}

func validateMiddlewareCall(descriptor GatewayMiddlewareDescriptor, method string, call MiddlewareCall) error {
	if len(call.Body) > MaxMiddlewareBodyBytes || gatewayState(call.State) != nil {
		return &Error{Code: "invalid_params", Message: "invalid or undeclared middleware call"}
	}
	if method == MethodMiddlewareAccount {
		if len(call.Body) != 0 || call.Context.Account == nil {
			return &Error{Code: "invalid_params", Message: "account hook requires only a sanitized account"}
		}
	} else if !gatewayObject(call.Body) {
		return &Error{Code: "invalid_params", Message: "middleware body must be a bounded JSON object"}
	}
	if err := gatewayContext(GatewayHookDescriptor{Origins: descriptor.Origins}, call.Context.GatewayContext); err != nil {
		return err
	}
	context := call.Context
	for _, value := range []string{context.Protocol, context.Model, context.Path, context.Agent,
		context.Session, context.Cwd, context.Kind, context.Event} {
		if !middlewareString(value, 4096) {
			return &Error{Code: "invalid_params", Message: "invalid middleware context"}
		}
	}
	const queryLimit = 16 << 10
	if len(context.Query) > queryLimit || len(context.Query) > queryLimit/3 &&
		!utf8.ValidString(context.Query) && len(string([]rune(context.Query))) > queryLimit ||
		context.Status != 0 && (context.Status < 100 || context.Status > 599) {
		return &Error{Code: "invalid_params", Message: "invalid middleware context bounds"}
	}
	if account := context.Account; account != nil {
		if len(account.Ref) != 32 || strings.ContainsFunc(account.Ref, func(value rune) bool {
			return (value < '0' || value > '9') && (value < 'a' || value > 'f')
		}) ||
			!middlewareString(account.Provider, 300) || !middlewareString(account.Name, 256) ||
			!middlewareString(account.Kind, 128) || !middlewareString(account.Model, 4096) {
			return &Error{Code: "invalid_params", Message: "invalid sanitized middleware account"}
		}
	}
	if provider := context.Provider; provider != nil {
		if !middlewareString(provider.ID, 300) || !middlewareString(provider.Name, 256) ||
			!middlewareString(provider.Account, 128) || !middlewareString(provider.Model, 4096) {
			return &Error{Code: "invalid_params", Message: "invalid middleware provider"}
		}
	}
	if method == MethodMiddlewareProvider && context.Provider == nil {
		return &Error{Code: "invalid_params", Message: "provider hook requires its current try"}
	}
	return nil
}

func DecodeMiddlewareResult(method string, raw json.RawMessage) (MiddlewareResult, error) {
	var result MiddlewareResult
	if !IsMiddlewareMethod(method) || decodeParams(raw, &result) != nil ||
		len(result.Body) > MaxMiddlewareBodyBytes || len(result.Body) != 0 && !gatewayObject(result.Body) ||
		gatewayState(result.State) != nil {
		return result, &Error{Code: "invalid_params", Message: "invalid bounded middleware result"}
	}
	if result.Reject != nil {
		reject := result.Reject
		if method != MethodMiddlewareRequest || reject.Status < 400 || reject.Status > 599 ||
			!searchString(reject.Message, 1024) || strings.TrimSpace(reject.Message) == "" ||
			len(result.Body) != 0 || result.Skip || result.Drop {
			return result, &Error{Code: "invalid_params", Message: "only request middleware may reject"}
		}
	}
	if result.Skip && method != MethodMiddlewareAccount || result.Drop && method != MethodMiddlewareEvent ||
		method == MethodMiddlewareAccount && len(result.Body) != 0 ||
		result.Drop && len(result.Body) != 0 {
		return result, &Error{Code: "invalid_params", Message: "middleware result exceeds its hook authority"}
	}
	return result, nil
}

func middlewareHandler(descriptor GatewayMiddlewareDescriptor, method string, handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if len(call.Auth) != 0 && string(call.Auth) != "null" {
			return nil, &Error{Code: "permission_denied", Message: "middleware cannot receive credentials"}
		}
		if _, err := DecodeMiddlewareCall(descriptor, method, call.Scope, call.Params); err != nil {
			return nil, err
		}
		result, err := handler(ctx, call, stream)
		if err != nil {
			return nil, err
		}
		if result == nil {
			result = MiddlewareResult{}
		}
		raw, err := marshalPayload(result)
		if err != nil {
			return nil, err
		}
		return DecodeMiddlewareResult(method, raw)
	}
}
