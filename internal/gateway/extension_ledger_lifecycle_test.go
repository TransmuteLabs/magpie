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
	"sync"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionLedgerDrainBarrier struct {
	rows    chan usage.Record
	release <-chan struct{}
}

func (*extensionLedgerDrainBarrier) Serve() func()                   { return func() {} }
func (*extensionLedgerDrainBarrier) Bodies() usage.Bodies            { return usage.NoBodies }
func (*extensionLedgerDrainBarrier) Begin(usage.Request) usage.Watch { return nil }
func (barrier *extensionLedgerDrainBarrier) Ledger(record usage.Record) {
	if record.Model == "m1" {
		barrier.rows <- record
		<-barrier.release
	}
}

func extensionLedgerObserverCount() int {
	count := 0
	for _, observer := range usage.Observers() {
		if fmt.Sprintf("%T", observer) == "*usage.extensionLedgerObserver" {
			count++
		}
	}
	return count
}

func TestExtensionLedgerObserverSurvivesListenerDrain(t *testing.T) {
	fresh(t)
	rows := make(chan sdk.LedgerObservation, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/ledger" {
			t.Errorf("unexpected collector request: %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		var row sdk.LedgerObservation
		if err := json.NewDecoder(request.Body).Decode(&row); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		rows <- row
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(collector.Close)
	manager := extensionhost.Default()
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(t.Context(), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "../usage/testdata/ledgerworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ledger fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "review.ledger", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.LedgerObserver}},
		Ledger:   &sdk.LedgerObservationDescriptor{TimeoutMs: 1000}}
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
	config, _ := json.Marshal(map[string]string{"sink": collector.URL + "/ledger"})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	releaseUpstream, entered := make(chan struct{}), make(chan struct{}, 1)
	var upstreamOnce sync.Once
	unblockUpstream := func() { upstreamOnce.Do(func() { close(releaseUpstream) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		entered <- struct{}{}
		<-releaseUpstream
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `{"id":"drain-response","choices":[{"message":{"role":"assistant","content":"drained"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(unblockUpstream)
	if err := provider.Save(provider.Provider{ID: "review", Name: "Review", Key: "synthetic",
		Chat: upstream.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	cancel, done := serveKeepAliveGateway(t, server)
	t.Cleanup(unblockUpstream)
	waitFor(t, "ledger observer on actual listener", func() bool {
		return server.Addr() != "" && extensionLedgerObserverCount() == 1
	})
	usage.Append(usage.Record{Provider: "review", Model: "probe-ready", Status: 200})
	select {
	case ready := <-rows:
		if ready.Model != "probe-ready" {
			t.Fatal("collector was not calibrated by the test", ready)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("installed ledger worker was not ready")
	}
	releaseTail := make(chan struct{})
	var tailOnce sync.Once
	unblockTail := func() { tailOnce.Do(func() { close(releaseTail) }) }
	barrier := &extensionLedgerDrainBarrier{rows: make(chan usage.Record, 1), release: releaseTail}
	removeBarrier := usage.Observe(barrier)
	t.Cleanup(removeBarrier)
	t.Cleanup(unblockTail)
	responseDone := make(chan error, 1)
	requestFinished := make(chan struct{})
	go func() {
		defer close(requestFinished)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"http://"+server.Addr()+"/v1/chat/completions",
			strings.NewReader(`{"model":"review/m1","messages":[{"role":"user","content":"drain proof"}]}`))
		if err != nil {
			responseDone <- err
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
		if err != nil {
			responseDone <- err
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err == nil && (response.StatusCode != 200 || !strings.Contains(string(body), "drained")) {
			err = fmt.Errorf("unexpected drain response: status=%d body=%s", response.StatusCode, body)
		}
		responseDone <- err
	}()
	t.Cleanup(func() {
		unblockUpstream()
		unblockTail()
		cancel()
		select {
		case <-requestFinished:
		case <-time.After(10 * time.Second):
			t.Error("review client cleanup did not join")
		}
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary Front did not dispatch the held request")
	}
	server.HandOff()
	cancel()
	removedEarly := false
	window := time.NewTimer(2 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
draining:
	for {
		if extensionLedgerObserverCount() == 0 {
			removedEarly = true
			break
		}
		select {
		case <-tick.C:
		case <-window.C:
			break draining
		case err := <-done:
			t.Fatalf("gateway stopped before its held request drained: %v", err)
		}
	}
	tick.Stop()
	window.Stop()
	if removedEarly {
		t.Errorf("SDK observer was removed while ordinary request remained in flight: requests=%d", server.Busy().Requests)
	}
	unblockUpstream()
	select {
	case tail := <-barrier.rows:
		t.Logf("ordinary terminal ledger row after listener cancellation: model=%s input=%d output=%d status=%d SDK observers=%d",
			tail.Model, tail.Input, tail.Output, tail.Status, extensionLedgerObserverCount())
		if !removedEarly {
			select {
			case observed := <-rows:
				if observed.Model != "m1" || observed.Input != 3 || observed.Output != 1 {
					t.Error("drained request changed in the ledger sink", observed)
				}
			case <-time.After(10 * time.Second):
				t.Error("drained terminal row never reached the still-serving SDK observer")
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held request never appended its ordinary terminal row")
	}
	unblockTail()
	select {
	case err := <-responseDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request cleanup did not join")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not join after its ledger writer returned")
	}
	if extensionLedgerObserverCount() != 0 {
		t.Error("gateway shutdown leaked an SDK observer")
	}
}
