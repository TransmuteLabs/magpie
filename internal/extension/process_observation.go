package extension

import (
	"log"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type ProcessObservation struct {
	RouteID     int64
	Scope       sdk.Scope
	Component   string
	Parent      string
	Incarnation string
	Phase       string
	Reason      string
	Millis      int64
	StdinBytes  uint64
	StdoutBytes uint64
	StderrBytes uint64
	Joined      bool
}

func (parent *ownedProcess) observe(phase string) {
	parent.mu.Lock()
	record := ProcessObservation{
		RouteID: parent.launch.RouteID, Scope: parent.scope, Component: parent.component,
		Parent: parent.parentID, Incarnation: parent.incarnation, Phase: phase, Reason: parent.reason,
		Millis: time.Since(parent.started).Milliseconds(), StdinBytes: parent.stdinBytes,
		StdoutBytes: parent.stdoutBytes, StderrBytes: parent.stderrBytes, Joined: parent.joined,
	}
	parent.mu.Unlock()
	manager := parent.lease.manager
	manager.mu.Lock()
	if manager.diagnostics == nil {
		manager.diagnostics = make(map[string]diagnosticWindow)
	}
	window := manager.diagnostics[record.Scope.Plugin]
	now := time.Now()
	if window.start.IsZero() || now.Sub(window.start) >= time.Minute {
		window = diagnosticWindow{start: now}
	}
	admitted := window.count < 60
	if admitted {
		window.count++
		manager.diagnostics[record.Scope.Plugin] = window
	}
	sink := manager.diagnosticLog
	manager.mu.Unlock()
	if admitted {
		value := moduleDiagnostic{
			Plugin: record.Scope.Plugin, Component: record.Component, Provider: record.Scope.Provider,
			Account: record.Scope.Account, Operation: record.Scope.Operation, Attempt: record.Scope.Attempt,
			Value:   sdk.Diagnostic{Level: "info", Code: "process_" + phase, Phase: "process", Millis: record.Millis},
			Process: &record,
		}
		if sink != nil {
			sink(value)
		} else {
			log.Printf("extension diagnostic: module=%q component=%q provider=%q account=%q operation=%q attempt=%d session=%q route=%d parent=%q process=%q phase=%s reason=%s millis=%d stdin=%d stdout=%d stderr=%d joined=%t",
				record.Scope.Plugin, record.Component, record.Scope.Provider, record.Scope.Account,
				record.Scope.Operation, record.Scope.Attempt, record.Scope.Session, record.RouteID, record.Parent,
				record.Incarnation, record.Phase, record.Reason, record.Millis, record.StdinBytes,
				record.StdoutBytes, record.StderrBytes, record.Joined)
		}
	}
	if parent.launch.Observe != nil {
		parent.launch.Observe(record)
	}
}
