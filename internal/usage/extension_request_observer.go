package usage

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionRequestObserver struct {
	extensionLedgerObserver
}

var extensionRequestSequence atomic.Uint64

func (*extensionRequestObserver) Ledger(Record) {}

func (observer *extensionRequestObserver) Begin(request Request) Watch {
	observer.mu.RLock()
	serving := observer.active
	observer.mu.RUnlock()
	if serving == nil {
		return nil
	}
	targets := serving.targets.Load()
	if targets == nil || len(*targets) == 0 {
		return nil
	}
	start := sdk.RequestObservation{
		Phase: "start", RequestID: strconv.FormatUint(extensionRequestSequence.Add(1), 16),
		Time: time.Now().UnixMilli(), Agent: observationAgent(request.Agent), Kind: observationKind(request.Kind),
	}
	watch := &extensionRequestWatch{observer: observer, serving: serving, start: start}
	for _, target := range *targets {
		if observer.enqueue(serving, extensionLedgerDelivery{target: target, request: start}) {
			watch.targets = append(watch.targets, target)
		}
	}
	if len(watch.targets) == 0 {
		return nil
	}
	return watch
}

type extensionRequestWatch struct {
	observer *extensionRequestObserver
	serving  *extensionLedgerServing
	start    sdk.RequestObservation
	targets  []extensionhost.LedgerTarget
	attempt  int
}

func (watch *extensionRequestWatch) Attempt(record Record) {
	watch.attempt++
	event := watch.event("attempt", record)
	event.Attempt = watch.attempt
	event.Provider = requestObservationIdentifier(record.Provider)
	event.Requested = requestObservationIdentifier(record.Requested)
	watch.deliver(event)
}

func (watch *extensionRequestWatch) End(record Record) {
	watch.deliver(watch.event("end", record))
}

func (watch *extensionRequestWatch) event(phase string, record Record) sdk.RequestObservation {
	event := watch.start
	event.Phase = phase
	if !record.Time.IsZero() {
		event.Time = record.Time.UnixMilli()
	}
	event.Model = requestObservationIdentifier(record.Model)
	event.Status = record.Status
	event.Millis = record.Millis
	return event
}

func (watch *extensionRequestWatch) deliver(event sdk.RequestObservation) {
	for _, target := range watch.targets {
		watch.observer.enqueue(watch.serving, extensionLedgerDelivery{target: target, request: event})
	}
}

// The gateway model and requested alias can originate in the client. Keep
// conventional identifiers, discarding whitespace, controls, and free text.
func requestObservationIdentifier(value string) string {
	if len(value) > 600 || strings.ContainsFunc(value, func(char rune) bool {
		return !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("._-/:+@", char))
	}) {
		return ""
	}
	return value
}

func observationKind(kind string) string {
	if _, known := purposeKinds[kind]; known || kind == "auxiliary" {
		return kind
	}
	return ""
}

// AgentOf can leave the product part of an unknown User-Agent verbatim.
func observationAgent(agent string) string {
	for _, known := range builtinAgents() {
		if known.ID == agent {
			return agent
		}
	}
	if IsLibrary(agent) && agent == strings.ToLower(agent) {
		return agent
	}
	return "other"
}
