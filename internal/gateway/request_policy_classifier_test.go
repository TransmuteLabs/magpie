package gateway

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
)

func TestSharedPolicyClassifierEffortFallbackKeepsRootBudget(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			fresh(t)
			upstream := thinkVendor(t, provider.Provider{ID: "classifier"}, "model", nil)
			upstream.refuse = func(body string) string {
				if strings.Contains(body, `"reasoning_effort"`) {
					return `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort","type":"invalid_request_error"}}`
				}
				return ""
			}
			ctx := ownPolicyContext(t, total)
			server := New()
			t.Cleanup(server.idle.stop)
			verdict, err := server.askWaited(ctx)(access.Identity{}, "classifier/model", []string{"refactoring", "tests"}, before{}, false, "rename the package")
			if sends := upstream.count(); sends != total+1 || (err == nil) != (total == 1) {
				t.Fatalf("total=%d sends=%d verdict=%+v err=%v", total, sends, verdict, err)
			}
			if used, selected := requestpolicy.FromContext(ctx).Counts("classifier"); used != total || selected != total {
				t.Fatal("classifier fallback reset root/provider budget", used, selected)
			}
		})
	}
}

func TestSharedPolicyClassifierQuestionsAreIndependentOperations(t *testing.T) {
	fresh(t)
	upstream := thinkVendor(t, provider.Provider{ID: "classifier"}, "model", nil)
	ctx := ownPolicyContext(t, 0)
	server := New()
	t.Cleanup(server.idle.stop)
	verdict, err := server.askWaited(ctx)(access.Identity{}, "classifier/model", []string{"refactoring", "tests"}, before{}, true, "rename the package")
	if err != nil || verdict.Intent != "refactoring" || verdict.Effort != "low" || upstream.count() != 2 {
		t.Fatalf("independent intent and effort questions: sends=%d verdict=%+v err=%v", upstream.count(), verdict, err)
	}
	if used, _ := requestpolicy.FromContext(ctx).Counts("classifier"); used != 0 {
		t.Fatal("independent classifier questions charged as retries", used)
	}
}

func TestSharedPolicyClassifierRetainsParentCancellation(t *testing.T) {
	fresh(t)
	upstream := thinkVendor(t, provider.Provider{ID: "classifier"}, "model", nil)
	ctx, cancel := context.WithCancel(ownPolicyContext(t, 1))
	cancel()
	server := New()
	t.Cleanup(server.idle.stop)
	_, err := server.askWaited(ctx)(access.Identity{}, "classifier/model", []string{"refactoring", "tests"}, before{}, false, "rename the package")
	if err == nil || upstream.count() != 0 {
		t.Fatalf("canceled parent dispatched classifier: sends=%d err=%v", upstream.count(), err)
	}
}

func TestSharedPolicyDecisionClassifierFallbackKeepsParentRoot(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			fresh(t)
			upstream := &decideFleet{status: map[string]int{"ka1": 503}}
			serverHTTP := upstream.serve()
			t.Cleanup(serverHTTP.Close)
			if err := provider.Save(provider.Provider{ID: "decision", Name: "Decision", Key: "ka1",
				Keys: []provider.KeyAccount{{Key: "ka2"}}, Routing: provider.Ordered,
				Decide: serverHTTP.URL + "/v1", Models: []string{"jev-latest"}}); err != nil {
				t.Fatal(err)
			}
			ctx := ownPolicyContext(t, total)
			server := New()
			t.Cleanup(server.idle.stop)
			verdict, err := server.askWaited(ctx)(access.Identity{}, "decision/jev-latest", []string{"bug"}, before{}, false, "fix this")
			asked := upstream.took()
			if len(asked) != total+1 || asked[0] != "ka1" || (err == nil) != (total == 1) {
				t.Fatalf("total=%d asked=%v verdict=%+v err=%v", total, asked, verdict, err)
			}
			if used, selected := requestpolicy.FromContext(ctx).Counts("decision"); used != total || selected != total {
				t.Fatal("decision classifier fallback reset root/provider budget", used, selected)
			}
		})
	}
}

func TestSharedPolicyDecisionCalibrationAndMainAreIndependent(t *testing.T) {
	fresh(t)
	upstream := &jevUp{choice: "bug", sure: 0.9}
	serverHTTP := httptest.NewServer(upstream)
	t.Cleanup(serverHTTP.Close)
	if err := provider.Save(provider.Provider{ID: "decision", Name: "Decision", Key: "key",
		Decide: serverHTTP.URL + "/v1", Models: []string{"jev-latest"}}); err != nil {
		t.Fatal(err)
	}
	ctx := ownPolicyContext(t, 0)
	server := New()
	t.Cleanup(server.idle.stop)
	verdict, err := server.askWaited(ctx)(access.Identity{}, "decision/jev-latest", []string{"feature", "bug"}, before{}, false, "fix this")
	upstream.mu.Lock()
	sends := len(upstream.asked)
	upstream.mu.Unlock()
	if err != nil || verdict.Intent != "bug" || sends != 2 {
		t.Fatalf("independent calibration and main: sends=%d verdict=%+v err=%v", sends, verdict, err)
	}
	if used, _ := requestpolicy.FromContext(ctx).Counts("decision"); used != 0 {
		t.Fatal("calibration or main first dispatch charged as retry", used)
	}
}

func TestSharedPolicyDecisionClassifierRetainsParentCancellation(t *testing.T) {
	fresh(t)
	upstream := &jevUp{choice: "bug", sure: 0.9}
	serverHTTP := httptest.NewServer(upstream)
	t.Cleanup(serverHTTP.Close)
	if err := provider.Save(provider.Provider{ID: "decision", Name: "Decision", Key: "key",
		Decide: serverHTTP.URL + "/v1", Models: []string{"jev-latest"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ownPolicyContext(t, 1))
	cancel()
	server := New()
	t.Cleanup(server.idle.stop)
	_, err := server.askWaited(ctx)(access.Identity{}, "decision/jev-latest", []string{"bug"}, before{}, false, "fix this")
	upstream.mu.Lock()
	sends := len(upstream.asked)
	upstream.mu.Unlock()
	if err == nil || sends != 0 {
		t.Fatalf("canceled parent dispatched decision classifier: sends=%d err=%v", sends, err)
	}
}

func TestSharedPolicyDecisionGroupKeepsParentRoot(t *testing.T) {
	for _, total := range []int{0, 1, -1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			fresh(t)
			upstream := &decideFleet{status: map[string]int{"first": 503}}
			serverHTTP := upstream.serve()
			t.Cleanup(serverHTTP.Close)
			for _, selected := range []struct{ id, key string }{
				{"decision-a", "first"}, {"decision-b", "second"},
			} {
				if err := provider.Save(provider.Provider{ID: selected.id, Name: selected.id, Key: selected.key,
					Decide: serverHTTP.URL + "/v1", Models: []string{"jev-latest"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := provider.SaveGroup(provider.Group{ID: "decision-children", Name: "Decision children",
				Members: []string{"decision-a/jev-latest", "decision-b/jev-latest"}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(ownPolicyContext(t, max(total, 0)))
			defer cancel()
			expected := total + 1
			if total < 0 {
				cancel()
				expected = 0
			}
			server := New()
			t.Cleanup(server.idle.stop)
			verdict, err := server.askWaited(ctx)(access.Identity{}, "group/decision-children", []string{"bug"}, before{}, false, "fix this")
			asked := upstream.took()
			if len(asked) != expected || expected > 0 && asked[0] != "first" || (err == nil) != (total == 1) {
				t.Fatalf("total=%d asked=%v verdict=%+v err=%v", total, asked, verdict, err)
			}
			if used, selected := requestpolicy.FromContext(ctx).Counts("decision-b"); used != max(total, 0) || selected != max(total, 0) {
				t.Fatal("decision group changed parent root/provider counters", used, selected)
			}
		})
	}
}
