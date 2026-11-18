package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const (
	RequestObserver      Capability = "request-observer"
	MethodRequestObserve            = "request.observe"
)

type RequestObservationDescriptor struct {
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

// RequestObservation describes one point in the life of an ordinary gateway
// request. RequestID is an opaque process-local identifier, shared only by
// this request's observations. Time is Unix milliseconds; on attempts it is
// the attempt's start, and on end it is the request's start.
type RequestObservation struct {
	Phase     string `json:"phase"`
	RequestID string `json:"requestId"`
	Time      int64  `json:"time"`
	Agent     string `json:"agent,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Requested string `json:"requested,omitempty"`
	Status    int    `json:"status,omitempty"`
	Millis    int64  `json:"millis,omitempty"`
}

func (manifest Manifest) validateRequestObserver() error {
	declared := manifest.Contract.Has(RequestObserver)
	if !declared && manifest.RequestObserver == nil {
		return nil
	}
	if !declared || manifest.RequestObserver == nil || manifest.Component != nil || manifest.Contract.Version.Minor < 12 {
		return errors.New("request observation requires SDK API 1.12 and a primary request-observer declaration")
	}
	if timeout := manifest.RequestObserver.TimeoutMs; timeout != 0 && (timeout < 10 || timeout > 1000) {
		return errors.New("request observer timeout must be 10..1000 milliseconds or zero")
	}
	return nil
}

func ValidateRequestObservation(event RequestObservation) error {
	if event.RequestID == "" || len(event.RequestID) > 16 ||
		strings.Trim(event.RequestID, "0123456789abcdef") != "" ||
		event.Time <= 0 || event.Time > 1<<53-1 || event.Status < 0 || event.Status > 599 ||
		event.Millis < 0 || event.Millis > 1<<53-1 || event.Attempt < 0 || int64(event.Attempt) > 1<<53-1 {
		return errors.New("invalid request observation")
	}
	for _, label := range []string{event.Agent, event.Kind, event.Provider, event.Model, event.Requested} {
		if !validExecutionText(label, 600) {
			return errors.New("invalid request observation metadata")
		}
	}
	switch event.Phase {
	case "start":
		if event.Attempt == 0 && event.Provider == "" && event.Model == "" &&
			event.Requested == "" && event.Status == 0 && event.Millis == 0 {
			return nil
		}
	case "attempt":
		if event.Attempt > 0 {
			return nil
		}
	case "end":
		if event.Attempt == 0 && event.Provider == "" && event.Requested == "" {
			return nil
		}
	}
	return errors.New("invalid request observation phase or fields")
}

func DecodeRequestObservation(raw json.RawMessage) (RequestObservation, error) {
	invalid := &Error{Code: "invalid_params", Message: "invalid sanitized request observation"}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return RequestObservation{}, invalid
	}
	for field := range fields {
		switch field {
		case "phase", "requestId", "time", "agent", "kind", "attempt", "provider", "model", "requested", "status", "millis":
		default:
			return RequestObservation{}, invalid
		}
	}
	var event RequestObservation
	if json.Unmarshal(raw, &event) != nil || ValidateRequestObservation(event) != nil {
		return RequestObservation{}, invalid
	}
	return event, nil
}

func ValidateRequestObservationScope(scope Scope) error {
	if scope != (Scope{Plugin: scope.Plugin}) {
		return &Error{Code: "permission_denied", Message: "request observation has no account, session or write authority"}
	}
	return nil
}

func ValidateRequestObservationResult(raw json.RawMessage) error {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || len(value) != 0 {
		return &Error{Code: "invalid_result", Message: "request observation cannot change the request or ledger"}
	}
	return nil
}

func requestObservationHandler(handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.Auth != nil {
			return nil, &Error{Code: "permission_denied", Message: "request observation has no credential scope"}
		}
		if err := ValidateRequestObservationScope(call.Scope); err != nil {
			return nil, err
		}
		if _, err := DecodeRequestObservation(call.Params); err != nil {
			return nil, err
		}
		result, err := handler(ctx, call, stream)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, &Error{Code: "invalid_result", Message: "invalid request observation result"}
		}
		return struct{}{}, ValidateRequestObservationResult(raw)
	}
}
