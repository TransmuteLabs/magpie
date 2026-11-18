package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"unsafe"

	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func finalRefusalWallStep(t *testing.T, observed time.Time, elapsed, wallStep time.Duration) time.Time {
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

func TestFinalRequestRefusalClockDomains(t *testing.T) {
	for _, scenario := range []string{
		"typed-response", "delta-header", "fractional-header", "duration-header",
		"absolute-response", "absolute-date-header", "mixed-response",
		"relative-response-absolute-window", "absolute-response-relative-window",
	} {
		t.Run(scenario, func(t *testing.T) {
			for _, step := range []struct {
				name              string
				elapsed, wallStep time.Duration
			}{
				{"forward-before-elapsed", 0, time.Hour},
				{"backward-after-elapsed", 45 * time.Second, -time.Hour},
			} {
				t.Run(step.name, func(t *testing.T) {
					scope := sdk.Scope{Plugin: "clock-fixture", Provider: "vendor", Account: "alice", Generation: 1, Operation: "clock", Attempt: 1}
					binding := &extensionAccount{
						host: &fixtureExtensionProviderHost{namespace: t.TempDir()},
						manifest: sdk.Manifest{Contract: sdk.Contract{
							Version: sdk.APIVersion{Major: 1, Minor: 12}, Capabilities: []sdk.Capability{sdk.RequestPolicy},
						}},
						scope: scope,
					}
					typed := int64(40000)
					facts := sdk.RequestFailureFacts{Status: 429, Code: "quota", Operation: sdk.MethodProviderExecute, Phase: "response", Complete: true}
					var windows []sdk.RequestFailureWindow
					var relative time.Duration
					var absolute time.Time
					switch scenario {
					case "typed-response":
						facts.RetryAfterMS, relative = &typed, 40*time.Second
					case "delta-header":
						facts.Headers, relative = map[string]string{"Retry-After": "40"}, 40*time.Second
					case "fractional-header":
						facts.Headers, relative = map[string]string{"Retry-After": "40.25"}, 40250*time.Millisecond
					case "duration-header":
						facts.Headers, relative = map[string]string{"X-Ratelimit-Reset-Requests": "40s"}, 40*time.Second
					case "absolute-response", "mixed-response", "absolute-response-relative-window":
						absolute = time.Now().Add(time.Minute).UTC()
						facts.ResetAt = absolute.Format(time.RFC3339Nano)
						if scenario == "mixed-response" {
							facts.RetryAfterMS, relative = &typed, 40*time.Second
						}
						if scenario == "absolute-response-relative-window" {
							windows = []sdk.RequestFailureWindow{{ID: "relative", Models: []string{"free"}, RetryAfterMS: &typed}}
							relative = 40 * time.Second
						}
					case "absolute-date-header":
						absolute = time.Now().Add(time.Minute).UTC().Truncate(time.Second)
						facts.Headers = map[string]string{"Retry-After": absolute.Format(http.TimeFormat)}
					case "relative-response-absolute-window":
						absolute = time.Now().Add(time.Minute).UTC()
						facts.RetryAfterMS, relative = &typed, 40*time.Second
						windows = []sdk.RequestFailureWindow{{ID: "absolute", Models: []string{"free"}, ResetAt: absolute.Format(time.RFC3339Nano)}}
					}
					result := sdk.RefusedExecution(sdk.RequestRecoveryRequest{
						Step: "clock", Facts: facts, Evidence: "rejected-before-effect",
						Rules: []sdk.RequestFailureRule{{ID: "quota", Code: "quota", Class: "quota", Scope: "model"}},
					}, windows)
					raw, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					ctx := requestpolicy.WithConfig(context.Background(), nil)
					refusal, err := binding.finalRequestRefusal(ctx, scope, "free", result, raw, false)
					if err != nil || refusal == nil || !refusal.NoEffect {
						t.Fatalf("valid selected final refusal was rejected: refusal=%+v err=%v", refusal, err)
					}
					if refusal.NextAllowed.Location() != time.UTC || refusal.ResponseNotBefore.Location() != time.UTC {
						t.Fatal("public final timing is not canonical UTC")
					}
					now := finalRefusalWallStep(t, refusal.ObservedAt, step.elapsed, step.wallStep)
					want := max(time.Duration(0), relative-step.elapsed)
					if !absolute.IsZero() {
						want = max(want, absolute.Sub(now.UTC()))
					}
					if got := requestpolicy.RetryMinimum(refusal.Facts(now), now); got != want {
						t.Fatalf("provider fact-to-refusal bridge lost a clock domain: got=%v want=%v", got, want)
					}
					cached, blocked := extensionAvailability.Blocked(binding.cacheKey(), "free", now)
					if blocked != (want > 0) {
						t.Fatalf("provider cache eligibility changed clock domains: blocked=%v want delay=%v refusal=%+v", blocked, want, cached)
					}
					if blocked && requestpolicy.RetryMinimum(cached.Facts(now), now) != want {
						t.Fatalf("provider cache lost an independent bound: want=%v refusal=%+v", want, cached)
					}
				})
			}
		})
	}
}
