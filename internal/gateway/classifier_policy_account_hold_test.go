package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
)

func TestClassifierPolicyAccountHoldsKeepRootBudgetAndCancellation(t *testing.T) {
	for _, model := range []string{"decision/jev-latest", "group/decision-held"} {
		for _, hold := range []string{"caller", "group"} {
			for _, total := range []int{0, 1, -1} {
				t.Run(fmt.Sprintf("%s/%s/total%d", model, hold, total), func(t *testing.T) {
					fresh(t)
					upstream := &decideFleet{status: map[string]int{"first": http.StatusServiceUnavailable}}
					serverHTTP := upstream.serve()
					t.Cleanup(serverHTTP.Close)
					if err := provider.Save(provider.Provider{ID: "decision", Name: "Decision", Key: "unheld",
						Keys: []provider.KeyAccount{{Key: "first"}, {Key: "second"}}, Routing: provider.Ordered,
						Decide: serverHTTP.URL + "/v1", Models: []string{"jev-latest"}}); err != nil {
						t.Fatal(err)
					}
					if err := provider.SaveGroup(provider.Group{ID: "decision-held", Name: "Decision held",
						Routing: provider.Ordered, Members: []string{"decision/jev-latest"}}); err != nil {
						t.Fatal(err)
					}
					accounts := []string{"decision/" + provider.KeyID("first"), "decision/" + provider.KeyID("second")}
					ctx, cancel := context.WithCancel(ownPolicyContext(t, max(total, 0)))
					defer cancel()
					held := access.Identity{}
					if hold == "caller" {
						ctx = access.WithIdentity(ctx, access.Identity{KeyID: "client", Accounts: accounts})
					} else {
						held = access.Identity{Group: "Routing hold", Accounts: accounts}
					}
					want := []string{"first"}
					if total == 1 {
						want = append(want, "second")
					}
					if total < 0 {
						cancel()
						want = nil
					}
					server := New()
					t.Cleanup(server.idle.stop)
					verdict, err := server.askWaited(ctx)(held, model, []string{"bug"}, before{}, false, "fix this")
					if asked := upstream.took(); !slices.Equal(asked, want) || (err == nil) != (total == 1) {
						t.Fatalf("held=%s total=%d asked=%v want=%v verdict=%+v err=%v", hold, total, asked, want, verdict, err)
					}
					if err == nil && verdict.Intent != "bug" {
						t.Fatalf("held=%s verdict=%+v, want bug", hold, verdict)
					}
					if used, selected := requestpolicy.FromContext(ctx).Counts("decision"); used != max(total, 0) || selected != max(total, 0) {
						t.Fatalf("held=%s total=%d lost parent counters: root=%d provider=%d", hold, total, used, selected)
					}
				})
			}
		}
	}
}
