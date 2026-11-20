package usage

import (
	"slices"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type (
	Observer = sdk.LinkedUsageObserver[Record]
	Watch    = sdk.LinkedUsageWatch[Record]
	Request  = sdk.LinkedUsageRequest
	Bodies   = sdk.LinkedUsageBodies
)

// CoversLedger reports whether the watch replaces this request's ordinary
// ledger observation. SDK request events use an independent delivery path.
func CoversLedger(w Watch) bool {
	_, sdkRequest := w.(*extensionRequestWatch)
	return !sdkRequest
}

const (
	NoBodies       = sdk.LinkedNoBodies
	CapturedBodies = sdk.LinkedCapturedBodies
	WholeBodies    = sdk.LinkedWholeBodies
)

var (
	obsMu     sync.Mutex
	observers []*observing
	serving   int // ServeObservers calls not yet stopped
)

type observing struct {
	o    Observer
	stop func() // while serving
}

// Observe registers o until remove is called. While this process serves
// the gateway it is started at once.
func Observe(o Observer) (remove func()) {
	obsMu.Lock()
	defer obsMu.Unlock()
	ob := &observing{o: o}
	if serving > 0 {
		ob.stop = o.Serve()
	}
	observers = append(observers, ob)
	var once sync.Once
	return func() {
		once.Do(func() {
			obsMu.Lock()
			observers = slices.DeleteFunc(slices.Clone(observers), func(x *observing) bool { return x == ob })
			stop := ob.stop
			ob.stop = nil
			obsMu.Unlock()
			if stop != nil {
				stop()
			}
		})
	}
}

// ServeObservers starts every observer registered, and each one
// registered later, until stop: the gateway calls it as it starts serving.
func ServeObservers() (stop func()) {
	obsMu.Lock()
	serving++
	if serving == 1 {
		for _, ob := range observers {
			ob.stop = ob.o.Serve()
		}
	}
	obsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			obsMu.Lock()
			serving--
			var stops []func()
			if serving == 0 {
				for _, ob := range observers {
					if ob.stop != nil {
						stops = append(stops, ob.stop)
						ob.stop = nil
					}
				}
			}
			obsMu.Unlock()
			for _, stop := range stops {
				stop()
			}
		})
	}
}

// Observers are the observers registered now.
func Observers() []Observer {
	obsMu.Lock()
	defer obsMu.Unlock()
	out := make([]Observer, len(observers))
	for i, ob := range observers {
		out[i] = ob.o
	}
	return out
}

// ObservedBodies is the most of a request's bodies any observer wants now.
func ObservedBodies() Bodies {
	b := NoBodies
	for _, o := range Observers() {
		b = max(b, o.Bodies())
	}
	return b
}

func observeLedger(r Record) {
	for _, o := range Observers() {
		o.Ledger(r)
	}
}

// BodyCut ends a body the gateway captured only the start of.
const BodyCut = "\n… (cut here by magpie)"
