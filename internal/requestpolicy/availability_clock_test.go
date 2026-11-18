package requestpolicy

import (
	"testing"
	"time"
	"unsafe"
)

func availabilityWallStep(t *testing.T, observed time.Time, elapsed, wallStep time.Duration) time.Time {
	t.Helper()
	type timeLayout struct {
		wall      uint64
		monotonic int64
		location  *time.Location
	}
	if unsafe.Sizeof(time.Time{}) != unsafe.Sizeof(timeLayout{}) {
		t.Fatal("pinned Go time layout changed")
	}
	current := observed.Add(elapsed)
	stepped := current.Add(wallStep)
	(*timeLayout)(unsafe.Pointer(&stepped)).monotonic = (*timeLayout)(unsafe.Pointer(&current)).monotonic
	if stepped.Sub(observed) != elapsed || stepped.UTC().Sub(observed.UTC()) != elapsed+wallStep {
		t.Fatal("clock fixture did not separate elapsed and wall time")
	}
	return stepped
}

func TestAvailabilityClockDomains(t *testing.T) {
	observed := time.Now()
	duration := int64(40000)
	for _, scenario := range []struct {
		name     string
		relative bool
		absolute time.Duration
	}{
		{"relative", true, 0},
		{"absolute", false, 40 * time.Second},
		{"mixed", true, time.Minute},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, step := range []struct {
				name              string
				elapsed, wallStep time.Duration
			}{
				{"initial", 0, 0},
				{"forward-before-elapsed", 0, time.Hour},
				{"backward-after-elapsed", 45 * time.Second, -time.Hour},
				{"elapsed-boundary", 40 * time.Second, 0},
				{"absolute-boundary", time.Minute, 0},
				{"both-passed", 65 * time.Second, time.Hour},
			} {
				t.Run(step.name, func(t *testing.T) {
					window := Window{ID: "free", Models: []string{"free"}}
					var want time.Duration
					if scenario.relative {
						window.RetryAfterMS = &duration
						want = max(want, 40*time.Second-step.elapsed)
					}
					if scenario.absolute > 0 {
						window.ResetAt = observed.Add(scenario.absolute).UTC().Format(time.RFC3339Nano)
						want = max(want, scenario.absolute-step.elapsed-step.wallStep)
					}
					var availability Availability
					availability.Remember("selected", Refusal{
						Classification: Classification{Class: Quota, Scope: "model", Rule: "clock"},
						NoEffect:       true, Model: "free", Status: 429, ObservedAt: observed, Windows: []Window{window},
					})
					now := availabilityWallStep(t, observed, step.elapsed, step.wallStep)
					refusal, blocked := availability.Blocked("selected", "free", now)
					if blocked != (want > 0) {
						t.Fatalf("wrong clock-domain eligibility: want remaining=%v blocked=%v refusal=%+v", want, blocked, refusal)
					}
					if blocked {
						if refusal.NextAllowed.Location() != time.UTC || !refusal.NextAllowed.Equal(now.Add(want)) {
							t.Fatalf("public timestamp did not project the active bounds: want=%v refusal=%+v", now.Add(want).UTC(), refusal)
						}
						if got := RetryMinimum(refusal.Facts(now), now); got != want {
							t.Fatalf("cached facts lost a clock-domain bound: got=%v want=%v", got, want)
						}
					}
				})
			}
		})
	}
}

func TestAvailabilityClockWindowsAndRefreshKeepBothBounds(t *testing.T) {
	observed := time.Now()
	longDuration, shortDuration := int64(40000), int64(10000)
	for _, refresh := range []bool{false, true} {
		name := "separate-windows"
		if refresh {
			name = "same-id-refresh"
		}
		t.Run(name, func(t *testing.T) {
			for _, step := range []struct {
				name              string
				elapsed, wallStep time.Duration
				want              time.Duration
			}{
				{"relative-remains", 0, time.Hour, 40 * time.Second},
				{"absolute-remains", 45 * time.Second, -time.Hour, time.Hour + 15*time.Second},
			} {
				t.Run(step.name, func(t *testing.T) {
					var availability Availability
					refusal := Refusal{
						Classification: Classification{Class: Quota, Scope: "model", Rule: "clock"},
						NoEffect:       true, Model: "free", Status: 429, ObservedAt: observed,
						Windows: []Window{{ID: "relative", Models: []string{"free"}, RetryAfterMS: &longDuration}},
					}
					availability.Remember("selected", refusal)
					windowID := "absolute"
					if refresh {
						windowID = "relative"
					}
					refusal.Windows = []Window{{
						ID: windowID, Models: []string{"free"}, RetryAfterMS: &shortDuration,
						ResetAt: observed.Add(time.Minute).UTC().Format(time.RFC3339Nano),
					}}
					availability.Remember("selected", refusal)
					now := availabilityWallStep(t, observed, step.elapsed, step.wallStep)
					blocked, ok := availability.Blocked("selected", "free", now)
					if !ok || RetryMinimum(blocked.Facts(now), now) != step.want {
						t.Fatalf("independent bounds were lost across windows/refresh: want=%v blocked=%v refusal=%+v", step.want, ok, blocked)
					}
				})
			}
		})
	}
}

func TestAvailabilityClockPruningPreservesRelativeFloor(t *testing.T) {
	observed := time.Now()
	duration := int64(40000)
	var availability Availability
	refusal := Refusal{
		Classification: Classification{Class: Quota, Scope: "model", Rule: "clock"},
		NoEffect:       true, Model: "free", ObservedAt: observed,
		Windows: []Window{{ID: "relative", RetryAfterMS: &duration}},
	}
	availability.Remember("selected", refusal)
	refusal.ObservedAt = availabilityWallStep(t, observed, 0, time.Hour)
	refusal.Windows = []Window{{ID: "unknown"}}
	availability.Remember("unrelated", refusal)
	if _, blocked := availability.Blocked("selected", "free", refusal.ObservedAt); !blocked {
		t.Fatal("unrelated observation pruned an unspent elapsed floor after a wall step")
	}
}

func TestAvailabilityClockFactsPreserveSubMillisecondFloor(t *testing.T) {
	observed := time.Now()
	duration := int64(40000)
	var availability Availability
	availability.Remember("selected", Refusal{
		Classification: Classification{Class: Quota, Scope: "model", Rule: "clock"},
		NoEffect:       true, Model: "free", Status: 429, ObservedAt: observed,
		Windows: []Window{{ID: "relative", RetryAfterMS: &duration}},
	})
	now := availabilityWallStep(t, observed, 11*time.Nanosecond, -time.Hour)
	refusal, blocked := availability.Blocked("selected", "free", now)
	if !blocked || RetryMinimum(refusal.Facts(now), now) != 40*time.Second-11*time.Nanosecond {
		t.Fatalf("duration floor was rounded or converted into a wall reset: blocked=%v refusal=%+v", blocked, refusal)
	}
}
