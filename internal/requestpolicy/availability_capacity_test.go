package requestpolicy

import (
	"fmt"
	"testing"
	"time"
)

func TestAvailabilityRefreshesExistingWindowAtCapacity(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var availability Availability
	refusal := Refusal{
		Classification: Classification{Class: Quota, Scope: "model", Rule: "quota"},
		Model:          "free",
		NoEffect:       true,
		ObservedAt:     now,
	}
	for index := range 64 {
		refusal.Windows = append(refusal.Windows, Window{
			ID:      fmt.Sprintf("window-%02d", index),
			Models:  []string{"free"},
			ResetAt: now.Add(time.Minute).Format(time.RFC3339),
		})
	}
	availability.Remember("alice-generation1", refusal)
	refusal.Windows = []Window{{
		ID:      "window-00",
		Models:  []string{"free"},
		ResetAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	availability.Remember("alice-generation1", refusal)
	if blocked, ok := availability.Blocked("alice-generation1", "free", now.Add(2*time.Minute)); !ok || !blocked.NextAllowed.Equal(now.Add(time.Hour)) {
		t.Fatalf("full cache lost the existing window's later reset: blocked=%t refusal=%+v", ok, blocked)
	}
	for _, selection := range []struct{ account, model string }{
		{"alice-generation1", "paid"},
		{"alice-generation2", "free"},
		{"bob-generation1", "free"},
	} {
		if _, ok := availability.Blocked(selection.account, selection.model, now); ok {
			t.Fatalf("window refresh blocked an unrelated selection: %+v", selection)
		}
	}
	if _, ok := availability.Blocked("alice-generation1", "free", now.Add(time.Hour)); ok {
		t.Fatal("refreshed window remained blocked after its reset")
	}
}
