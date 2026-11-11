package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestZCodeQuotaCommandsRejectMissingSelectionAndInternalOperations(t *testing.T) {
	for _, args := range [][]string{
		{"quota", "reset", "--provider", "zcode", "--window", "week", "--yes"},
		{"quota", "reset", "--provider", "zcode", "--account", "a", "--window", "month", "--yes"},
		{"quota", "reset", "--provider", "zcode", "--account", "a", "--window", "fiveHour", "--yes"},
		{"quota", "reset", "--provider", "zcode", "--account", "a", "--window", "weekly", "--yes"},
		{"quota", "account", "zcode", "a", "mcp.headers", "--yes"},
		{"quota", "account", "zcode", "a", "reset.use", "--yes"},
		{"quota", "account", "zcode", "a", "offpeak.ticket", "--yes"},
		{"quota", "account", "zcode", "a", "highspeed.draw", "--yes"},
		{"quota", "account", "zcode", "a", "selection.select", "--params", "{"},
	} {
		if err := quotaCmd(args); err == nil {
			t.Errorf("accepted %s", strings.Join(args, " "))
		}
	}
}

func TestZCodeResetCancellationDoesNotCallVendor(t *testing.T) {
	groupsHome(t)
	var contexts, spends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == provider.RemoteZCodeAccountPath {
			contexts.Add(1)
			_, _ = w.Write([]byte(`{"context":{"scope":"scope","generation":"epoch"},"data":{}}`))
			return
		}
		spends.Add(1)
		http.Error(w, "unexpected spend", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "fixture", Chat: server.URL}); err != nil {
		t.Fatal(err)
	}
	piped(t, "no")
	_, err := stdoutOf(t, func() error {
		return quotaCmd([]string{"quota", "reset", "--provider", "office/zcode", "--account", "a", "--window", "week"})
	})
	if err == nil || contexts.Load() != 1 || spends.Load() != 0 {
		t.Fatalf("cancel = %v, context=%d spends=%d", err, contexts.Load(), spends.Load())
	}
}

func TestZCodeResetConfirmationUsesCapturedRemoteContext(t *testing.T) {
	groupsHome(t)
	var contexts, spends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == provider.RemoteZCodeAccountPath {
			contexts.Add(1)
			_, _ = w.Write([]byte(`{"context":{"scope":"scope-a","generation":"epoch-a"},"data":{}}`))
			return
		}
		if r.URL.Path != provider.RemoteZCodeResetPath+"/scoped" {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		spends.Add(1)
		var body struct{ Scope, Generation, Window string }
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Scope != "scope-a" || body.Generation != "epoch-a" || body.Window != "five-hour" {
			t.Errorf("confirmation did not retain captured selection: %+v", body)
		}
		_, _ = w.Write([]byte(`{"source":"zcode","available":true,"pending":"unconfirmed","pendingWindow":"five-hour"}`))
	}))
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "fixture", Chat: server.URL}); err != nil {
		t.Fatal(err)
	}
	piped(t, "yes")
	out, err := stdoutOf(t, func() error {
		return quotaCmd([]string{"quota", "reset", "--provider", "office/zcode", "--account", "a", "--window", "five-hour"})
	})
	if err != nil || contexts.Load() != 1 || spends.Load() != 1 || !strings.Contains(out, "unconfirmed") {
		t.Fatalf("confirmation: context=%d spends=%d output=%s err=%v", contexts.Load(), spends.Load(), out, err)
	}
}

func TestZCodeQuotaShowsUnavailablePendingReset(t *testing.T) {
	quota := provider.Quota{
		Provider: "zcode",
		User:     "account",
		Kind:     "subscription",
		ZCodeReset: &provider.ZCodeResetState{
			Source: "zcode", Available: false, Error: "status-unreachable",
			Pending: "unconfirmed", PendingWindow: "week",
		},
	}
	out, err := stdoutOf(t, func() error {
		printQuotas([]provider.Quota{quota})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"status-unreachable", "unconfirmed", "week"} {
		if !strings.Contains(out, value) {
			t.Errorf("quota output omits %q: %s", value, out)
		}
	}
	if strings.Contains(out, "five-hour") || strings.Contains(out, "weekly") {
		t.Fatalf("unavailable pending state presented card counts: %s", out)
	}
}

func TestZCodeQuotaShowsCountsOnlyWhenAvailableAndNotPending(t *testing.T) {
	expires := float64(time.Now().Add(time.Hour).UnixMilli())
	for _, pending := range []bool{false, true} {
		state := &provider.ZCodeResetState{Source: "zcode", Available: true, FiveHour: []float64{expires, expires}, Week: []float64{expires}}
		if pending {
			state.Pending, state.PendingWindow = "unconfirmed", "week"
		}
		out, err := stdoutOf(t, func() error {
			printQuotas([]provider.Quota{{Provider: "zcode", User: "account", Kind: "subscription", ZCodeReset: state}})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if pending {
			if !strings.Contains(out, "unconfirmed") || !strings.Contains(out, "week") || strings.Contains(out, "five-hour") || strings.Contains(out, "weekly") {
				t.Errorf("pending reset presented counts or hid the window: %s", out)
			}
		} else if !strings.Contains(out, "2 five-hour") || !strings.Contains(out, "1 weekly") {
			t.Errorf("available counts not visible: %s", out)
		}
	}
}
