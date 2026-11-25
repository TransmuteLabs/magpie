package gateway

import "time"

// expirySweep schedules at most one timer to run a store's TTL cleanup when
// its earliest retained entry expires. Every method requires the store's lock.
type expirySweep struct {
	timer *time.Timer
	due   time.Time
	gen   uint64
}

func (s *expirySweep) arm(delay time.Duration, fn func(uint64)) {
	if delay < 0 {
		delay = 0
	}
	delay += time.Millisecond
	due := time.Now().Add(delay)
	if s.timer != nil && !due.Before(s.due) {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.gen++
	gen := s.gen
	s.due = due
	s.timer = time.AfterFunc(delay, func() { fn(gen) })
}

func (s *expirySweep) stop() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
		s.due = time.Time{}
		s.gen++
	}
}

func (s *expirySweep) fire(gen uint64) bool {
	if s.gen != gen {
		return false
	}
	s.timer = nil
	s.due = time.Time{}
	return true
}
