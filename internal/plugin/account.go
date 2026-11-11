package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// AccountContext identifies an account selection and its login generation;
// it contains no credentials.
type AccountContext struct {
	Scope           string `json:"scope"`
	Generation      string `json:"generation"`
	Origin          string `json:"origin"`
	Family          string `json:"family"`
	UserID          string `json:"userId"`
	AccountIdentity string `json:"accountIdentity,omitempty"`
	Kind            string `json:"kind"`
	Organization    string `json:"organization"`
	Project         string `json:"project"`
	Product         string `json:"product"`
}

// AccountRequest identifies the operation and, after context capture, the
// exact selection and generation the caller intends to operate on.
type AccountRequest struct {
	Operation  string `json:"operation"`
	Params     any    `json:"params"`
	Scope      string `json:"scope"`
	Generation string `json:"generation"`
}

type AccountResult struct {
	Context AccountContext  `json:"context"`
	Data    json.RawMessage `json:"data"`
}

// AccountError carries only fields safe to expose from an account hook.
type AccountError struct {
	Kind       string `json:"kind,omitempty"`
	Code       int    `json:"code,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	Throttled  bool   `json:"throttled,omitempty"`
	Dispatched bool   `json:"-"`
	// DispatchedKnown is set when a boolean dispatched value was received.
	// A true Dispatched value is also known when constructed directly in Go.
	DispatchedKnown bool    `json:"-"`
	RequestID       string  `json:"requestId,omitempty"`
	NextTakeAt      int64   `json:"nextTakeAt,omitempty"`
	NextDrawAt      int64   `json:"nextDrawAt,omitempty"`
	RetryAfter      float64 `json:"retryAfter,omitempty"`
	Message         string  `json:"-"`
}

func (e AccountError) MarshalJSON() ([]byte, error) {
	type fields AccountError
	var dispatched *bool
	if e.DispatchedKnown || e.Dispatched {
		dispatched = &e.Dispatched
	}
	return json.Marshal(struct {
		fields
		Dispatched *bool `json:"dispatched,omitempty"`
	}{fields(e), dispatched})
}

func (e *AccountError) UnmarshalJSON(data []byte) error {
	type fields AccountError
	var wire struct {
		fields
		Dispatched *bool `json:"dispatched"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = AccountError(wire.fields)
	if wire.Dispatched != nil {
		e.Dispatched, e.DispatchedKnown = *wire.Dispatched, true
	}
	return nil
}

func (e *AccountError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Kind != "" {
		return e.Kind
	}
	return "plugin account operation failed"
}

// AccountOperation invokes an account hook for one explicitly selected
// existing account, through its proxy and with the caller's cancellation.
func AccountOperation(ctx context.Context, provider, account string, request AccountRequest) (AccountResult, error) {
	var result AccountResult
	if request.Operation != "mcp.headers" && (strings.TrimSpace(provider) == "" || strings.TrimSpace(account) == "" || ProviderOf(account) != provider) {
		return result, errors.New("an explicit account of the provider is required")
	}
	if strings.TrimSpace(request.Operation) == "" {
		return result, errors.New("an account operation is required")
	}
	err := Call(ctx, "account", map[string]any{
		"provider": provider, "account": account, "proxy": proxyOf(ctx),
		"operation": request.Operation, "params": request.Params,
		"scope": request.Scope, "generation": request.Generation,
	}, &result)
	return result, err
}
