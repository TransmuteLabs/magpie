package usage

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/beat"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionLedgerQueue = 256

type extensionLedgerObserver struct {
	mu      sync.RWMutex
	manager *extensionhost.Manager
	owner   context.Context
	request bool
	active  *extensionLedgerServing
	dropped atomic.Uint64
	failed  atomic.Uint64
}

type extensionLedgerServing struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	queue   chan extensionLedgerDelivery
	targets atomic.Pointer[[]extensionhost.LedgerTarget]
}

type extensionLedgerDelivery struct {
	target  extensionhost.LedgerTarget
	row     sdk.LedgerObservation
	request sdk.RequestObservation
}

func (*extensionLedgerObserver) Bodies() Bodies      { return NoBodies }
func (*extensionLedgerObserver) Begin(Request) Watch { return nil }

func (observer *extensionLedgerObserver) Serve() func() {
	observer.mu.Lock()
	if observer.active != nil {
		observer.mu.Unlock()
		return func() {}
	}
	manager := observer.manager
	if manager == nil {
		manager = extensionhost.Default()
	}
	owner := observer.owner
	if owner == nil {
		owner = context.Background()
	}
	ctx, cancel := context.WithCancel(owner)
	serving := &extensionLedgerServing{ctx: ctx, cancel: cancel, done: make(chan struct{}),
		queue: make(chan extensionLedgerDelivery, extensionLedgerQueue)}
	observer.active = serving
	observer.mu.Unlock()
	observer.refresh(serving, manager)
	go observer.run(serving, manager)
	var once sync.Once
	return func() {
		once.Do(func() {
			observer.mu.Lock()
			if observer.active == serving {
				observer.active = nil
			}
			observer.mu.Unlock()
			cancel()
			<-serving.done
		})
	}
}

func (observer *extensionLedgerObserver) refresh(serving *extensionLedgerServing, manager *extensionhost.Manager) {
	var targets []extensionhost.LedgerTarget
	var err error
	if observer.request {
		targets, err = manager.RequestObservationTargets(serving.ctx)
	} else {
		targets, err = manager.LedgerTargets(serving.ctx)
	}
	if err != nil {
		targets = nil
	}
	serving.targets.Store(&targets)
}

func (observer *extensionLedgerObserver) run(serving *extensionLedgerServing, manager *extensionhost.Manager) {
	defer close(serving.done)
	written := filememo.Written()
	for {
		select {
		case <-serving.ctx.Done():
			return
		case <-written:
			written = filememo.Written()
			observer.refresh(serving, manager)
		case <-beat.Next(filememo.OthersEvery()):
			observer.refresh(serving, manager)
		case delivery := <-serving.queue:
			ctx, cancel := context.WithTimeout(serving.ctx, delivery.target.Timeout)
			var err error
			if observer.request {
				err = manager.ObserveRequest(ctx, delivery.target, delivery.request)
			} else {
				err = manager.ObserveLedger(ctx, delivery.target, delivery.row)
			}
			cancel()
			if err != nil && serving.ctx.Err() == nil {
				if failed := observer.failed.Add(1); failed%1000 == 1 {
					log.Printf("SDK %s observer %s: delivery unavailable; failed=%d", observer.kind(), delivery.target.Module, failed)
				}
			}
		}
	}
}

func (observer *extensionLedgerObserver) Ledger(record Record) {
	observer.mu.RLock()
	serving := observer.active
	observer.mu.RUnlock()
	if serving == nil {
		return
	}
	targets := serving.targets.Load()
	if targets == nil || len(*targets) == 0 {
		return
	}
	row := extensionLedgerRow(record)
	if sdk.ValidateLedgerObservation(row) != nil {
		if failed := observer.failed.Add(1); failed%1000 == 1 {
			log.Printf("SDK ledger observer: invalid projected record; failed=%d", failed)
		}
		return
	}
	for _, target := range *targets {
		observer.enqueue(serving, extensionLedgerDelivery{target: target, row: row})
	}
}

func (observer *extensionLedgerObserver) kind() string {
	if observer.request {
		return "request"
	}
	return "ledger"
}

func (observer *extensionLedgerObserver) enqueue(serving *extensionLedgerServing, delivery extensionLedgerDelivery) bool {
	select {
	case <-serving.ctx.Done():
		return false
	case serving.queue <- delivery:
		return true
	default:
		if dropped := observer.dropped.Add(1); !observer.request && dropped%1000 == 1 {
			log.Printf("SDK ledger observer %s: queue full; dropped=%d", delivery.target.Module, dropped)
		}
		return false
	}
}

func extensionLedgerRow(record Record) sdk.LedgerObservation {
	row := sdk.LedgerObservation{
		Time: record.Time.UnixMilli(), Operation: ledgerObservationMetadata(record.Operation),
		Agent: ledgerObservationMetadata(record.Agent), Provider: ledgerObservationMetadata(record.Provider),
		Model: ledgerObservationMetadata(record.Model), Requested: ledgerObservationMetadata(record.Requested),
		Served: ledgerObservationMetadata(record.Served),
		Input:  int64(record.Input), Output: int64(record.Output), CacheRead: int64(record.CacheRead),
		CacheWrite: int64(record.CacheWrite), CacheWrite1h: int64(record.CacheWrite1h), CacheUnknown: record.CacheUnknown,
		Reasoning: int64(record.Reasoning), Millis: record.Millis, TTFT: record.TTFT, Status: record.Status,
		ErrorType: ledgerObservationMetadata(record.ErrType), Kind: ledgerObservationMetadata(record.Kind),
		Rejected: record.Rejected, HandedOn: record.HandedOn, Tries: record.Tries,
	}
	if row.Agent == "" && record.Agent != "" {
		row.Agent = "other"
	}
	if record.Credits != nil {
		credits := *record.Credits
		row.Credits = &credits
	}
	if record.OwnCost != nil {
		cost := *record.OwnCost
		row.CostUSD = &cost
	}
	if record.ReportedCost != nil {
		row.ReportedCost = &sdk.ExecutionCost{Amount: record.ReportedCost.Amount, Currency: record.ReportedCost.Currency}
	}
	return row
}

func ledgerObservationMetadata(value string) string {
	if len(value) > sdk.LedgerObservationMetadataLimit || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return ""
	}
	return value
}
