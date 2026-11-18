package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionRequestObserverOrdinaryFallback(t *testing.T) {
	fresh(t)
	events := make(chan []byte, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		events <- raw
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(collector.Close)
	manager := extensionhost.Default()
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "../usage/testdata/ledgerworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build request observer fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "review.request-observer", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract:        sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 12}, Capabilities: []sdk.Capability{sdk.RequestObserver}},
		RequestObserver: &sdk.RequestObservationDescriptor{TimeoutMs: 1000}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"sink": collector.URL})
	firstConfig, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})

	targets, err := manager.RequestObservationTargets()
	if err != nil || len(targets) != 1 {
		t.Fatal("request observer discovery", targets, err)
	}
	probe := sdk.RequestObservation{Phase: "start", RequestID: "abc", Time: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	warm := func(target extensionhost.LedgerTarget) {
		t.Helper()
		if err := manager.ObserveRequest(ctx, target, probe); err != nil {
			t.Fatalf("request observer worker probe: %v", err)
		}
		select {
		case raw := <-events:
			event, err := sdk.DecodeRequestObservation(raw)
			if err != nil || event != probe {
				t.Fatal("unexpected observer calibration", event, err)
			}
		case <-ctx.Done():
			t.Fatal("observer did not deliver its calibrated probe")
		}
	}
	warm(targets[0])
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, firstConfig.Version, config); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveRequest(ctx, targets[0], probe); err == nil {
		t.Fatal("stale request event acquired the replacement generation")
	}
	current, err := manager.RequestObservationTargets()
	if err != nil || len(current) != 1 || current[0] == targets[0] {
		t.Fatal("replacement request observer discovery", current, err)
	}
	warm(current[0])

	const primaryKey = "syntheticprimarycredential"
	const backupKey = "syntheticbackupcredential"
	const prompt = "syntheticprivateprompt"
	const reply = "syntheticprivatereply"
	const refusal = "syntheticprivatefailure"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer " + primaryKey:
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"`+refusal+`"}}`)
		case "Bearer " + backupKey:
			io.WriteString(w, `{"id":"upstream-private-id","model":"m1","choices":[{"message":{"role":"assistant","content":"`+reply+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":3}}`)
		default:
			t.Error("ordinary inference did not send its selected provider credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: primaryKey,
		Chat: up.URL + "/v1", Models: []string{"m1"}, Keys: []provider.KeyAccount{{Key: backupKey, Name: "syntheticbackupkeylabel"}}}); err != nil {
		t.Fatal(err)
	}
	keys, secrets := newCaller(t, "syntheticcallerkeylabel")
	tap := &fakeObserver{}
	t.Cleanup(usage.Observe(tap))
	server := New()
	t.Cleanup(server.idle.stop)
	serveKeepAliveGateway(t, server)
	waitFor(t, "request observer on actual listener", func() bool {
		for _, observer := range usage.Observers() {
			if fmt.Sprintf("%T", observer) == "*usage.extensionRequestObserver" {
				return server.Addr() != ""
			}
		}
		return false
	})
	if usage.ObservedBodies() != usage.NoBodies {
		t.Fatal("SDK request observation enabled body capture")
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+server.Addr()+"/v1/chat/completions",
		strings.NewReader(`{"model":"plan/m1","messages":[{"role":"user","content":"`+prompt+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+secrets[0])
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "python-httpx/0.28.1")
	r.Header.Set(SessionHeader, "syntheticprivateroutingsession")
	r.Header.Set("X-Session-Id", "syntheticprivatenativesession")
	r.Header.Set("X-Proof", "syntheticprivateheader")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), reply) {
		t.Fatalf("ordinary response status=%d body=%s error=%v", resp.StatusCode, body, err)
	}
	var requestID string
	wantPhases := []string{"start", "attempt", "attempt", "end"}
	wantStatuses := []int{0, 429, 200, 200}
	wantAttempts := []int{0, 1, 2, 0}
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for index, want := range wantPhases {
		select {
		case raw := <-events:
			event, err := sdk.DecodeRequestObservation(raw)
			if err != nil {
				t.Fatalf("invalid wire event %q: %v", raw, err)
			}
			if index == 0 {
				requestID = event.RequestID
			}
			if event.RequestID == "" || event.RequestID != requestID || event.Agent != "python-httpx" ||
				event.Phase != want || event.Status != wantStatuses[index] || event.Attempt != wantAttempts[index] {
				t.Fatalf("event %d: %+v", index, event)
			}
			if want == "attempt" && (event.Provider != "plan" || event.Model != "m1" || event.Requested != "plan/m1") {
				t.Fatalf("attempt %d lost routing metadata: %+v", index, event)
			}
			for _, private := range []string{primaryKey, backupKey, secrets[0], prompt, reply, refusal,
				"upstream-private-id", "syntheticcallerkeylabel", "syntheticbackupkeylabel", "syntheticprivateroutingsession", "syntheticprivatenativesession", "syntheticprivateheader", "python-httpx/0.28.1"} {
				if strings.Contains(string(raw), private) {
					t.Fatal("private request material crossed the worker boundary")
				}
			}
		case <-timeout.C:
			t.Fatalf("missing %s event after %d observations", want, index)
		}
	}
	tap.mu.Lock()
	defer tap.mu.Unlock()
	if len(tap.rows) != 1 || tap.rows[0].Watched || tap.rows[0].Status != 200 || tap.rows[0].Input != 17 ||
		tap.rows[0].Output != 3 || tap.rows[0].CallerKeyID != keys[0].ID || tap.rows[0].ProviderKeyID != provider.KeyID(backupKey) {
		t.Fatalf("request observation changed ordinary ledger coverage or accounting: %+v", tap.rows)
	}
}
