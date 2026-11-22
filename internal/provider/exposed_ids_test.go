package provider

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

// The quotas narrowed by the models offered build the lists of their own
// providers only: every other provider's, each account's and plugin's,
// was built on each usage read for nothing.
func TestExposedIDsBuildsOnlyTheProvidersAsked(t *testing.T) {
	t.Cleanup(AntigravityBuiltinForTest()) // the retired built-in route
	isolate(t)
	for _, p := range []Provider{
		{ID: "asked", Name: "Asked", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"gemini-a"}},
		{ID: "other", Name: "Other", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1"}},
	} {
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	got := exposedIDs(t.Context(), "asked", "antigravity")
	if _, ok := got["other"]; ok || len(got) != 1 {
		t.Fatalf("built %v, want the one asked alone", got)
	}
	if !got["asked"]["gemini-a"] {
		t.Fatalf("its offered models: %v", got["asked"])
	}
}

// The tray reads usage every few minutes: the offered models of the
// providers whose quotas are narrowed are built once while nothing changes, so the catalog they
// load can be let go, and again after a change, which reaches the answer.
func TestKeptExposedIDsBuiltOncePerChange(t *testing.T) {
	t.Cleanup(AntigravityBuiltinForTest()) // the retired built-in route
	isolate(t)
	keptExposed.Lock()
	keptExposed.ids = nil
	keptExposed.Unlock()
	if err := Save(Provider{ID: "asked", Name: "Asked", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"gemini-a"}}); err != nil {
		t.Fatal(err)
	}
	first := keptExposedIDs(t.Context(), "asked", "antigravity")
	if !first["asked"]["gemini-a"] {
		t.Fatalf("offered: %v", first["asked"])
	}
	builds := exposedBuilds.Load()
	for range 5 {
		keptExposedIDs(t.Context(), "asked", "antigravity")
	}
	if n := exposedBuilds.Load() - builds; n != 0 {
		t.Fatalf("built %d more times while nothing changed", n)
	}
	if err := Save(Provider{ID: "asked", Name: "Asked", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"gemini-b"}}); err != nil {
		t.Fatal(err)
	}
	got := keptExposedIDs(t.Context(), "asked", "antigravity")
	if !got["asked"]["gemini-b"] || got["asked"]["gemini-a"] {
		t.Fatalf("after the change: %v", got["asked"])
	}
}

func TestSubscriptionUsageGoogleExposureHonorsDeadline(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	card := SubscriptionQuota{Provider: "gemini", User: "fixture", Windows: []QuotaWindow{{Name: "daily", Used: 12}}}
	warmSubscriptionUsageForTest(t, card)
	Changed()
	release, held := filelock.Lock(filepath.Join(host.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the current SDK Store")
	}
	synctest.Test(t, func(t *testing.T) {
		done := make(chan []SubscriptionQuota, 1)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		go func() { done <- SubscriptionUsage(ctx) }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			release()
			if len(got) != 0 {
				t.Errorf("an expired request published Gemini quotas: %+v", got)
			}
		default:
			release()
			<-done
			t.Error("Gemini model visibility held the usage request past its deadline")
		}
	})
	if got := SubscriptionUsage(t.Context()); len(got) != 1 || got[0].Provider != "gemini" ||
		len(got[0].Windows) != 1 || got[0].Windows[0].Used != 12 {
		t.Fatalf("canceled discovery discarded the kept Gemini window: %+v", got)
	}
}
