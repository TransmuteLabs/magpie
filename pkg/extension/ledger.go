package extension

import (
	"context"
	"encoding/json"
	"errors"
)

const (
	LedgerObserver      Capability = "ledger-observer"
	MethodLedgerObserve            = "ledger.observe"
)

type LedgerObservationDescriptor struct {
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

type LedgerObservation struct {
	Time         int64          `json:"time"`
	Operation    string         `json:"operation,omitempty"`
	Agent        string         `json:"agent"`
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	Requested    string         `json:"requested,omitempty"`
	Served       string         `json:"served,omitempty"`
	Input        int64          `json:"input"`
	Output       int64          `json:"output"`
	CacheRead    int64          `json:"cacheRead"`
	CacheWrite   int64          `json:"cacheWrite"`
	CacheWrite1h int64          `json:"cacheWrite1h"`
	CacheUnknown bool           `json:"cacheUnknown"`
	Reasoning    int64          `json:"reasoning"`
	Millis       int64          `json:"millis"`
	TTFT         int64          `json:"ttft"`
	Status       int            `json:"status"`
	ErrorType    string         `json:"errorType,omitempty"`
	Kind         string         `json:"kind,omitempty"`
	Rejected     bool           `json:"rejected"`
	HandedOn     bool           `json:"handedOn"`
	Tries        int            `json:"tries"`
	Credits      *float64       `json:"credits,omitempty"`
	CostUSD      *float64       `json:"costUSD,omitempty"`
	ReportedCost *ExecutionCost `json:"reportedCost,omitempty"`
}

func (manifest Manifest) validateLedgerObserver() error {
	declared := manifest.Contract.Has(LedgerObserver)
	if !declared && manifest.Ledger == nil {
		return nil
	}
	if !declared || manifest.Ledger == nil || manifest.Component != nil || manifest.Contract.Version.Minor < 8 {
		return errors.New("ledger observation requires SDK API 1.8 and a primary ledger-observer declaration")
	}
	if timeout := manifest.Ledger.TimeoutMs; timeout != 0 && (timeout < 10 || timeout > 1000) {
		return errors.New("ledger observer timeout must be 10..1000 milliseconds or zero")
	}
	return nil
}

func ValidateLedgerObservation(row LedgerObservation) error {
	if row.Time <= 0 || row.Time > 1<<53-1 || row.Status < 0 || row.Status > 599 || row.Tries < 0 ||
		int64(row.Tries) > 1<<53-1 || row.CacheWrite1h > row.CacheWrite {
		return errors.New("invalid ledger observation")
	}
	for _, number := range []int64{row.Input, row.Output, row.CacheRead, row.CacheWrite,
		row.CacheWrite1h, row.Reasoning, row.Millis, row.TTFT} {
		if number < 0 || number > 1<<53-1 {
			return errors.New("invalid ledger observation counts")
		}
	}
	for _, text := range []string{row.Operation, row.Agent, row.Provider, row.Model, row.Requested, row.Served, row.ErrorType, row.Kind} {
		if !validExecutionText(text, 600) {
			return errors.New("invalid ledger observation metadata")
		}
	}
	for _, amount := range []*float64{row.Credits, row.CostUSD} {
		if amount != nil && !validExecutionAmount(*amount) {
			return errors.New("invalid ledger observation amount")
		}
	}
	if cost := row.ReportedCost; cost != nil && (!validExecutionAmount(cost.Amount) ||
		cost.Currency == "" || !validExecutionText(cost.Currency, 16)) {
		return errors.New("invalid ledger observation currency")
	}
	return nil
}

func ledgerHandler(handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.Auth != nil || call.Scope.Provider != "" || call.Scope.Account != "" || call.Scope.Generation != 0 ||
			call.Scope.Session != "" || call.Scope.Operation != "" || call.Scope.Attempt != 0 {
			return nil, &Error{Code: "permission_denied", Message: "ledger observation has no account, session or write authority"}
		}
		var row LedgerObservation
		if decodeParams(call.Params, &row) != nil || ValidateLedgerObservation(row) != nil {
			return nil, &Error{Code: "invalid_params", Message: "invalid sanitized ledger observation"}
		}
		result, err := handler(ctx, call, stream)
		if err != nil {
			return nil, err
		}
		var value map[string]json.RawMessage
		raw, err := json.Marshal(result)
		if err != nil || json.Unmarshal(raw, &value) != nil || len(value) != 0 {
			return nil, &Error{Code: "invalid_result", Message: "ledger observation cannot modify its row"}
		}
		return struct{}{}, nil
	}
}
