package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func ownPolicyContext(t *testing.T, total int) context.Context {
	t.Helper()
	zero, repeats := 0, 1
	current := settings.Load()
	current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
		MaxTotalRetries: &total, MaxRetries: &repeats, BaseDelayMS: &zero,
	}}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	ctx := withRequestPolicy(t.Context())
	policy, err := requestpolicy.FromContext(ctx).Policy("outer", requestpolicy.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := requestpolicy.FromContext(ctx).Dispatch(ctx, policy); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestSharedPolicyOwnSearchOperationsDoNotChargeFirstDispatch(t *testing.T) {
	_, next, _ := pickedSearchSetup(t, false, provider.Anthropic)
	current := settings.Load()
	current.Searcher = "ds2/deepseek-flash"
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	ctx := ownPolicyContext(t, 0)
	server := New()
	t.Cleanup(server.idle.stop)
	for index := range 3 {
		said, hits, err := server.webSearch(ctx, fmt.Sprintf("weather query %d", index))
		if err != nil || said == "" || len(hits) == 0 {
			t.Fatalf("search %d lost its first business dispatch: said=%q hits=%d err=%v", index, said, len(hits), err)
		}
	}
	if sends := len(next.searches()); sends != 3 {
		t.Fatal("independent searches changed", sends)
	}
	if total, _ := requestpolicy.FromContext(ctx).Counts("ds2"); total != 0 {
		t.Fatal("first search dispatch charged as retry", total)
	}
}

func TestSharedPolicyOwnSearchFallbackSpendsRootBudget(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			first, next, _ := pickedSearchSetup(t, false, provider.Anthropic)
			current := settings.Load()
			current.Searcher, current.SearcherThen = "ds/deepseek-flash", []string{"ds2/deepseek-flash"}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			ctx := ownPolicyContext(t, total)
			server := New()
			t.Cleanup(server.idle.stop)
			_, _, err := server.webSearch(ctx, "weather")
			if len(first.searches()) != 1 || len(next.searches()) != total || (err == nil) != (total == 1) {
				t.Fatalf("total=%d first=%d fallback=%d err=%v", total, len(first.searches()), len(next.searches()), err)
			}
			if used, _ := requestpolicy.FromContext(ctx).Counts("ds2"); used != total {
				t.Fatal("search fallback lost root accounting", used)
			}
		})
	}
}

func TestSharedPolicyOwnAdvisorOperationsDoNotChargeFirstDispatch(t *testing.T) {
	upstream := advisedSetup(t)
	ctx := ownPolicyContext(t, 0)
	server := New()
	t.Cleanup(server.idle.stop)
	for range 3 {
		said, err := server.advise(ctx, &Request{Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: "help"}}}}}, nil, "adv/adv-pro")
		if err != nil || !strings.Contains(said, "Check the tests") {
			t.Fatalf("advisor lost its first business dispatch: said=%q err=%v", said, err)
		}
	}
	upstream.mu.Lock()
	sends := len(upstream.advisor)
	upstream.mu.Unlock()
	if sends != 3 {
		t.Fatal("independent advice changed", sends)
	}
	if total, _ := requestpolicy.FromContext(ctx).Counts("adv"); total != 0 {
		t.Fatal("first advisor dispatch charged as retry", total)
	}
}

func TestSharedPolicyOwnAdvisorRetriesRetainRootBudget(t *testing.T) {
	upstream := advisedSetup(t)
	upstream.mu.Lock()
	upstream.fail = http.StatusTooManyRequests
	upstream.mu.Unlock()
	selected, err := provider.Find("adv")
	if err != nil {
		t.Fatal(err)
	}
	selected.Patience = provider.PatienceOff
	if err := provider.Save(*selected); err != nil {
		t.Fatal(err)
	}
	ctx := ownPolicyContext(t, 1)
	server := New()
	t.Cleanup(server.idle.stop)
	for range 2 {
		if _, err := server.advise(ctx, &Request{Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: "help"}}}}}, nil, "adv/adv-pro"); err == nil {
			t.Fatal("refused advice unexpectedly succeeded")
		}
	}
	upstream.mu.Lock()
	sends := len(upstream.advisor)
	upstream.mu.Unlock()
	if sends != 3 {
		t.Fatal("child retry allowance reset or first dispatch charged", sends)
	}
	if total, selected := requestpolicy.FromContext(ctx).Counts("adv"); total != 1 || selected != 1 {
		t.Fatal("advisor retries escaped shared root/provider counts", total, selected)
	}
}
