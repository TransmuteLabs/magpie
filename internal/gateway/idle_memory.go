package gateway

import (
	"context"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// A request's body is held several times over while it is served (as
// read, masked, captured for the log), and Go keeps the pages it freed
// after: a tray magpie with ~60 requests an hour sat at 725 MB of private
// working set after a collection, its heap holding 1088 MB of which 315 MB
// was live (#3472). Go's scavenger hands such pages back only slowly, and
// not while the next collection's goal covers them. So once the gateway
// has had nothing in flight for idleRelease, what its requests left is
// handed back at once, as the warm-up does with what its reads left.

// idleRelease is how long the gateway rests before that.
const idleRelease = 30 * time.Second

// idleMemory is a Server's release of idle memory: a test sets its rest
// and counts its releases.
type idleMemory struct {
	mu      sync.Mutex
	serving context.Context
	due     *idleMemoryTimer
	rest    time.Duration // idleRelease when zero
	release func()        // debug.FreeOSMemory when nil
}

type idleMemoryTimer struct {
	timer  *time.Timer
	server atomic.Pointer[Server]
}

func (m *idleMemory) start(ctx context.Context) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serving = ctx
	return m.stop
}

// stop calls off the release to come.
func (m *idleMemory) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serving = nil
	if m.due != nil {
		m.due.server.Store(nil)
		m.due.timer.Stop()
		m.due = nil
	}
}

func (m *idleMemory) restFor() time.Duration {
	if m.rest > 0 {
		return m.rest
	}
	return idleRelease
}

// armRelease schedules a release for the listening gateway after its rest.
func (s *Server) armRelease() {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	if s.idle.due == nil && s.idleServing() {
		s.scheduleIdleRelease(s.idle.restFor())
	}
}

func (s *Server) idleServing() bool {
	return s.idle.serving != nil && s.idle.serving.Err() == nil && !s.notAccepting.Load()
}

func (s *Server) scheduleIdleRelease(delay time.Duration) {
	due := &idleMemoryTimer{}
	due.server.Store(s)
	due.timer = time.AfterFunc(delay, due.run)
	s.idle.due = due
}

func (due *idleMemoryTimer) run() {
	if server := due.server.Swap(nil); server != nil {
		server.releaseIfResting(due)
	}
}

// releaseIfResting hands memory back when nothing has been in flight for
// idleRelease. A request in flight now arms it again when it ends, and a
// rest not long enough yet waits out the rest of it.
func (s *Server) releaseIfResting(due *idleMemoryTimer) {
	s.idle.mu.Lock()
	if s.idle.due != due {
		s.idle.mu.Unlock()
		return
	}
	s.idle.due = nil
	if !s.idleServing() || s.inFlight.Load() != 0 {
		s.idle.mu.Unlock()
		return
	}
	if wait := s.idle.restFor() - time.Since(time.Unix(0, s.lastDone.Load())); wait > 0 {
		s.scheduleIdleRelease(wait)
		s.idle.mu.Unlock()
		return
	}
	release := s.idle.release
	s.idle.mu.Unlock()
	if release != nil {
		release()
		return
	}
	debug.FreeOSMemory()
}
