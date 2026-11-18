package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	GatewayHooks Capability = "gateway-hooks"

	MethodGatewayRequest  = "gateway.request"
	MethodGatewayResponse = "gateway.response"
	MethodGatewaySignIn   = "gateway.signin"
	MaxGatewayStateBytes  = 64 << 10
)

type GatewayHookDescriptor struct {
	Request   bool     `json:"request,omitempty"`
	Response  bool     `json:"response,omitempty"`
	SignIn    bool     `json:"signIn,omitempty"`
	TimeoutMs int      `json:"timeoutMs,omitempty"`
	Origins   []string `json:"origins,omitempty"`
}

type GatewayContext struct {
	Origin   string `json:"origin"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Path     string `json:"path"`
	Agent    string `json:"agent"`
	Session  string `json:"session"`
	Cwd      string `json:"cwd"`
	Query    string `json:"query"`
	Kind     string `json:"kind"`
}

type GatewayRequest struct {
	Body    json.RawMessage `json:"body"`
	Context GatewayContext  `json:"context"`
}

type GatewayRequestResult struct {
	Body  json.RawMessage `json:"body,omitempty"`
	State json.RawMessage `json:"state,omitempty"`
}

type GatewayResponse struct {
	Request json.RawMessage `json:"request"`
	Reply   json.RawMessage `json:"reply"`
	Success bool            `json:"success"`
	State   json.RawMessage `json:"state,omitempty"`
	Context GatewayContext  `json:"context"`
}

type GatewaySignInEvent struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	Account      string `json:"account"`
	State        string `json:"state"`
	EndsAt       string `json:"endsAt,omitempty"`
}

type GatewaySignIn struct {
	Event GatewaySignInEvent `json:"event"`
}

func IsGatewayMethod(method string) bool {
	return method == MethodGatewayRequest || method == MethodGatewayResponse || method == MethodGatewaySignIn
}

func GatewayMethodEffect(method string) (string, bool) {
	switch method {
	case MethodGatewayRequest:
		return "read", true
	case MethodGatewayResponse, MethodGatewaySignIn:
		return "write", true
	default:
		return "", false
	}
}

func (descriptor GatewayHookDescriptor) AllowsOrigin(origin string) bool {
	if len(descriptor.Origins) == 0 {
		return origin == "local" || origin == "magpie-key"
	}
	for _, allowed := range descriptor.Origins {
		if allowed == origin {
			return true
		}
	}
	return false
}

func validGatewayOrigin(origin string) bool {
	return origin == "local" || origin == "magpie-key" || origin == "gateway-key" || origin == "remote" || origin == "browser"
}

func (manifest Manifest) validateHooks() error {
	for _, tool := range manifest.Tools {
		if tool.Exposure == "" {
			continue
		}
		if manifest.Contract.Version.Minor < 2 || tool.Exposure != "shared" && tool.Exposure != "local-owner" {
			return fmt.Errorf("tool exposure requires SDK API 1.2 and shared or local-owner")
		}
	}
	descriptor := manifest.Hooks
	if descriptor == nil && !manifest.Contract.Has(GatewayHooks) {
		return nil
	}
	if manifest.Contract.Version.Minor < 2 || descriptor == nil || !manifest.Contract.Has(GatewayHooks) {
		return fmt.Errorf("gateway hooks require SDK API 1.2, gateway-hooks and a hooks descriptor")
	}
	if manifest.Component != nil || !descriptor.Request && !descriptor.Response && !descriptor.SignIn {
		return fmt.Errorf("gateway hooks require at least one declared hook on the primary component")
	}
	if descriptor.TimeoutMs != 0 && (descriptor.TimeoutMs < 100 || descriptor.TimeoutMs > 30_000) {
		return fmt.Errorf("gateway hook timeout must be 100..30000 milliseconds or zero for the default")
	}
	seen := map[string]bool{}
	for _, origin := range descriptor.Origins {
		if !validGatewayOrigin(origin) || seen[origin] {
			return fmt.Errorf("unknown or duplicate gateway hook origin %q", origin)
		}
		seen[origin] = true
	}
	if descriptor.SignIn && !descriptor.AllowsOrigin("local") {
		return fmt.Errorf("sign-in hooks require local origin permission")
	}
	return nil
}

func gatewayObject(body json.RawMessage) bool {
	body = bytes.TrimSpace(body)
	return len(body) != 0 && body[0] == '{' && json.Valid(body)
}

func gatewayState(state json.RawMessage) error {
	if len(state) > MaxGatewayStateBytes {
		return &Error{Code: "resource_exhausted", Message: "gateway hook state exceeds its byte limit"}
	}
	if len(state) != 0 && !json.Valid(state) {
		return &Error{Code: "invalid_params", Message: "gateway hook state must be JSON"}
	}
	return nil
}

func gatewayContext(descriptor GatewayHookDescriptor, value GatewayContext) error {
	if !validGatewayOrigin(value.Origin) {
		return &Error{Code: "invalid_params", Message: "unknown gateway request origin"}
	}
	if !descriptor.AllowsOrigin(value.Origin) {
		return &Error{Code: "permission_denied", Message: "gateway origin is outside this hook's declaration"}
	}
	return nil
}

func gatewayParams(method string, params json.RawMessage, descriptor GatewayHookDescriptor) error {
	switch method {
	case MethodGatewayRequest:
		var request GatewayRequest
		if err := decodeParams(params, &request); err != nil {
			return err
		}
		if !gatewayObject(request.Body) {
			return &Error{Code: "invalid_params", Message: "gateway request body must be a JSON object"}
		}
		return gatewayContext(descriptor, request.Context)
	case MethodGatewayResponse:
		var response GatewayResponse
		if err := decodeParams(params, &response); err != nil {
			return err
		}
		if !gatewayObject(response.Request) || response.Success && !gatewayObject(response.Reply) ||
			len(response.Reply) != 0 && !json.Valid(response.Reply) {
			return &Error{Code: "invalid_params", Message: "invalid completed gateway exchange"}
		}
		if err := gatewayState(response.State); err != nil {
			return err
		}
		return gatewayContext(descriptor, response.Context)
	case MethodGatewaySignIn:
		var notification GatewaySignIn
		if err := decodeParams(params, &notification); err != nil {
			return err
		}
		event := notification.Event
		if event.Provider == "" || len(event.Provider) > 300 ||
			strings.ContainsFunc(event.Provider, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) ||
			len(event.ProviderName) > 256 || len(event.Account) > 256 ||
			strings.ContainsFunc(event.ProviderName+event.Account, unicode.IsControl) ||
			event.State != "signed-out" && event.State != "ending" {
			return &Error{Code: "invalid_params", Message: "invalid sanitized gateway sign-in event"}
		}
		if event.EndsAt != "" {
			if _, err := time.Parse(time.RFC3339, event.EndsAt); err != nil {
				return &Error{Code: "invalid_params", Message: "sign-in expiry must be an RFC3339 timestamp"}
			}
		}
	}
	return nil
}

func gatewayHandler(method string, handler Handler, descriptor GatewayHookDescriptor) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.Scope.Provider != "" || call.Scope.Account != "" || call.Scope.Generation != 0 ||
			call.Auth != nil {
			return nil, &Error{Code: "permission_denied", Message: "gateway hooks cannot receive provider, account or auth scope"}
		}
		if err := gatewayParams(method, call.Params, descriptor); err != nil {
			return nil, err
		}
		call.Auth = nil
		result, err := handler(ctx, call, stream)
		if err != nil || method != MethodGatewayRequest {
			return result, err
		}
		if result == nil {
			return GatewayRequestResult{}, nil
		}
		body, err := marshalPayload(result)
		if err != nil {
			return nil, err
		}
		var requestResult GatewayRequestResult
		if !gatewayObject(body) || decodeParams(body, &requestResult) != nil ||
			len(requestResult.Body) != 0 && !gatewayObject(requestResult.Body) {
			return nil, &Error{Code: "invalid_params", Message: "gateway request result requires optional object body and JSON state"}
		}
		if err := gatewayState(requestResult.State); err != nil {
			return nil, err
		}
		return requestResult, nil
	}
}
