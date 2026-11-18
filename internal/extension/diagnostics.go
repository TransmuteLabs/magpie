package extension

import (
	"encoding/json"
	"errors"
	"log"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	moduleDiagnosticLimit        = 60
	moduleDiagnosticErrorReserve = 10
)

type diagnosticWindow struct {
	start time.Time
	count int
}

type moduleDiagnostic struct {
	Plugin        string
	Component     string
	Provider      string
	Account       string
	Operation     string
	Attempt       uint64
	Value         sdk.Diagnostic
	Process       *ProcessObservation
	RequestPolicy *sdk.RequestRecoveryResult
}

func (m *Manager) emitDiagnostic(scope sdk.Scope, raw json.RawMessage, authority hostAuthority) (any, error) {
	var value sdk.Diagnostic
	if !decodeHostParams(raw, &value) || sdk.ValidateDiagnostic(authority.manifest, value) != nil {
		return nil, errors.New("invalid structured module diagnostic")
	}
	m.mu.Lock()
	if m.diagnostics == nil {
		m.diagnostics = make(map[string]diagnosticWindow)
	}
	window := m.diagnostics[scope.Plugin]
	now := time.Now()
	if window.start.IsZero() || now.Sub(window.start) >= time.Minute {
		window = diagnosticWindow{start: now}
	}
	limit := moduleDiagnosticLimit
	if value.Level != "error" {
		limit -= moduleDiagnosticErrorReserve
	}
	if window.count >= limit {
		m.mu.Unlock()
		return sdk.DiagnosticResult{Recorded: false}, nil
	}
	window.count++
	m.diagnostics[scope.Plugin] = window
	sink := m.diagnosticLog
	m.mu.Unlock()
	component := sdk.PrimaryComponent
	if authority.manifest.Component != nil {
		component = authority.manifest.Component.ID
	}
	record := moduleDiagnostic{Plugin: scope.Plugin, Component: component, Provider: scope.Provider,
		Account: scope.Account, Operation: scope.Operation, Attempt: scope.Attempt, Value: value}
	if sink != nil {
		sink(record)
	} else {
		retryable := "unknown"
		if value.Retryable != nil {
			retryable = "false"
			if *value.Retryable {
				retryable = "true"
			}
		}
		log.Printf("extension diagnostic: module=%q component=%q provider=%q account=%q operation=%q attempt=%d level=%s code=%s phase=%s status=%d millis=%d count=%d retryable=%s",
			record.Plugin, record.Component, record.Provider, record.Account, record.Operation, record.Attempt,
			value.Level, value.Code, value.Phase, value.Status, value.Millis, value.Count, retryable)
	}
	return sdk.DiagnosticResult{Recorded: true}, nil
}
