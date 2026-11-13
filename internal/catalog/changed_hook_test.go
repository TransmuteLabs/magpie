package catalog

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestChangedHookConcurrentNotifications(test *testing.T) {
	previous, forget := ChangedHook(), Forget
	test.Cleanup(func() { SetChanged(previous); Forget = forget })
	Forget = nil
	var calls atomic.Int64
	hook := func() { calls.Add(1) }
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for range 1000 {
			SetChanged(hook)
			SetChanged(nil)
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for range 1000 {
			Touched()
		}
	}()
	close(start)
	workers.Wait()
	SetChanged(hook)
	before := calls.Load()
	Touched()
	if calls.Load() != before+1 {
		test.Fatal("a current change hook was not called exactly once")
	}
	SetChanged(nil)
	Touched()
	if ChangedHook() != nil || calls.Load() != before+1 {
		test.Fatal("a cleared change hook was still called")
	}
}

func TestChangedHookCanReplaceItself(test *testing.T) {
	previous, forget := ChangedHook(), Forget
	test.Cleanup(func() { SetChanged(previous); Forget = forget })
	Forget = nil
	var called int
	SetChanged(func() {
		called++
		SetChanged(nil)
	})
	Touched()
	Touched()
	if called != 1 || ChangedHook() != nil {
		test.Fatal("a change hook could not replace itself during notification")
	}
}
