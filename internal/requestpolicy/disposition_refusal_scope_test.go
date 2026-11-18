package requestpolicy

import (
	"testing"
	"time"
)

func TestDispositionRefusalIsTerminalAcrossScopes(t *testing.T) {
	policy := Defaults()
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, class := range []Class{Request, Content} {
		for _, scope := range []string{"request", "model", "account", "unknown"} {
			for _, retryAfter := range []string{"", "1", "7200"} {
				t.Run(string(class)+"/"+scope+"/retry-after="+retryAfter, func(t *testing.T) {
					failure := Facts{Status: 400, Code: "3006", Complete: true, Operation: "provider.execute", Phase: "response"}
					classification := Classification{Class: class, Scope: scope, Rule: "vendor.refusal"}
					wantNextAllowed := time.Time{}
					if retryAfter != "" {
						failure.Headers = map[string]string{"Retry-After": retryAfter}
						delay, err := time.ParseDuration(retryAfter + "s")
						if err != nil {
							t.Fatal(err)
						}
						wantNextAllowed = now.Add(delay)
					}
					decision := policy.Disposition(failure, classification, Safety{NoEffect: true}, 0, now, 0)
					if decision.Action != ActionTerminal || decision.Retry || decision.Delay != 0 {
						t.Fatalf("refusal enumerates or schedules recovery: %+v", decision)
					}
					if decision.Classification != classification || !decision.NextAllowed.Equal(wantNextAllowed) {
						t.Fatalf("terminal refusal lost classification or timing diagnostics: %+v", decision)
					}
				})
			}
		}
	}
}

func TestDispositionRefusalNeighborEligibility(t *testing.T) {
	policy := Defaults()
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, class := range []Class{Auth, Quota} {
		for _, scope := range []string{"request", "model", "account", "unknown"} {
			for _, scenario := range []struct {
				name   string
				safety Safety
			}{
				{"rejected", Safety{NoEffect: true}},
				{"no-proof", Safety{}},
				{"accepted", Safety{NoEffect: true, Accepted: true}},
				{"uncertain", Safety{NoEffect: true, Uncertain: true}},
				{"output", Safety{NoEffect: true, OutputStarted: true}},
			} {
				t.Run(string(class)+"/"+scope+"/"+scenario.name, func(t *testing.T) {
					failure := Facts{Status: 403, Complete: true, Operation: "provider.execute", Phase: "response"}
					classification := Classification{Class: class, Scope: scope, Rule: "vendor.eligible"}
					wantAction := ActionTerminal
					if scenario.name == "rejected" && (scope == "model" || scope == "account") {
						wantAction = ActionNext
					}
					decision := policy.Disposition(failure, classification, scenario.safety, 0, now, 0)
					if decision.Action != wantAction || decision.Retry || decision.Classification != classification {
						t.Fatalf("neighbor eligibility changed: want %s, got %+v", wantAction, decision)
					}
				})
			}
		}
	}
}
