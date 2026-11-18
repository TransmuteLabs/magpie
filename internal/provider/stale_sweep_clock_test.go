package provider

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
)

func TestStaleSweepClockIsTimeZoneIndependent(test *testing.T) {
	if os.Getenv("MAGPIE_STALE_CLOCK_CHILD") != "1" {
		command := proc.CommandContext(test.Context(), os.Args[0], "-test.run=^TestStaleSweepClockIsTimeZoneIndependent$", "-test.count=1")
		command.Env = append(os.Environ(), "MAGPIE_STALE_CLOCK_CHILD=1")
		if output, err := command.CombinedOutput(); err != nil {
			test.Fatalf("isolated stale clock regression: %v\n%s", err, output)
		}
		return
	}
	testenv.Zone(test, time.Local)
	first := time.FixedZone("first", 3600)
	second := time.FixedZone("second", -3600)
	start := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		<-start
		for index := 0; index < 5000; index++ {
			time.Local = first
			runtime.Gosched()
			time.Local = second
		}
	}()
	go func() {
		defer writers.Done()
		<-start
		for index := 0; index < 5000; index++ {
			staleFor.Lock()
			staleArmLocked(10 * time.Millisecond)
			staleFor.Unlock()
			runtime.Gosched()
		}
	}()
	close(start)
	writers.Wait()
}
