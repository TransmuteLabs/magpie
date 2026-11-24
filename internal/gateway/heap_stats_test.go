package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
)

const sampleHeapPath = "/v1/magpie/heap/stats"

func TestHeapStatsWithoutCollection(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	t.Setenv(AllowIPsEnv, "")
	server := httptest.NewServer(New().Front())
	defer server.Close()
	client := &http.Client{Timeout: 20 * time.Second}
	started, err := proc.Started(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	forced := []metrics.Sample{{Name: "/gc/cycles/forced:gc-cycles"}}
	sample := func() {
		t.Helper()
		metrics.Read(forced)
		forcedBefore := forced[0].Value.Uint64()
		before := time.Now()
		response, err := client.Get(server.URL + sampleHeapPath)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("heap stats status: got %d, want 200", response.StatusCode)
		}
		if response.Header.Get("Content-Type") != "application/json" ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get(HeapStatsHeader) != "" {
			t.Fatalf("heap stats headers: %v", response.Header)
		}
		var stats struct {
			PID                                                 int
			Version                                             string
			ProcessStartedAt, SampleStartedAt, SampleFinishedAt time.Time
			HeapObjectBytes, HeapInUseBytes, HeapIdleBytes      uint64
			HeapReleasedBytes, HeapSysBytes, RuntimeSysBytes    uint64
			GCCycles, Goroutines                                uint64
			LastGCLiveBytes, ForcedGCCycles                     *uint64
		}
		if err := json.Unmarshal(body, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.PID != os.Getpid() || stats.Version != Version || !stats.ProcessStartedAt.Equal(started) ||
			stats.SampleStartedAt.Before(before) || stats.SampleFinishedAt.Before(stats.SampleStartedAt) ||
			stats.SampleFinishedAt.After(after) {
			t.Fatalf("heap stats identity or sample time: %s", body)
		}
		if stats.HeapObjectBytes == 0 || stats.HeapInUseBytes < stats.HeapObjectBytes ||
			stats.HeapIdleBytes < stats.HeapReleasedBytes || stats.HeapSysBytes != stats.HeapInUseBytes+stats.HeapIdleBytes ||
			stats.RuntimeSysBytes < stats.HeapSysBytes || stats.LastGCLiveBytes == nil || stats.Goroutines == 0 {
			t.Fatalf("heap stats counters: %s", body)
		}
		metrics.Read(forced)
		if stats.ForcedGCCycles == nil || *stats.ForcedGCCycles != forcedBefore ||
			stats.GCCycles < forcedBefore || forced[0].Value.Uint64() != forcedBefore {
			t.Fatalf("a stats read changed the forced collection count: %s", body)
		}
	}

	sample()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		stuckSave(t, held, release)
	}()
	defer func() {
		close(release)
		<-done
	}()
	<-held
	sample()
}

func TestHeapStatsOnlyDirectLoopback(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	t.Setenv("MAGPIE_TRUST_PROXY", "1")
	keys, secrets := newCaller(t, "Full", "Desk display")
	if _, err := access.Update("access-key", access.Change{Key: keys[1].ID, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	settingsWithOrigin := settings.Load()
	settingsWithOrigin.CORSOrigins = []string{"https://listed.example"}
	if err := settings.Save(settingsWithOrigin); err != nil {
		t.Fatal(err)
	}
	server := New()
	call := func(method, path, remote, host string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, nil)
		request.RemoteAddr = remote
		request.Host = host
		for i := 0; i < len(hdr); i += 2 {
			request.Header.Set(hdr[i], hdr[i+1])
		}
		response := httptest.NewRecorder()
		server.Front().ServeHTTP(response, request)
		return response
	}
	check := func(name string, response *httptest.ResponseRecorder) {
		t.Helper()
		if (response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized) ||
			strings.Contains(response.Body.String(), `"heapObjectBytes"`) {
			t.Errorf("%s: %d %q", name, response.Code, response.Body.String())
		}
	}
	for _, caller := range profileCallersRefused(secrets[0]) {
		check(caller.name, call(http.MethodGet, sampleHeapPath, caller.remote, caller.host, caller.hdr...))
	}

	remote, host := "127.0.0.1:50123", "127.0.0.1:3425"
	check("page port", call(http.MethodGet, sampleHeapPath, remote, host, PagePortHeader, "1"))
	check("full key", call(http.MethodGet, sampleHeapPath, remote, host, "Authorization", "Bearer "+secrets[0]))
	check("header key", call(http.MethodGet, sampleHeapPath, remote, host, "x-api-key", secrets[0]))
	check("query key", call(http.MethodGet, sampleHeapPath+"?key="+secrets[0], remote, host))
	check("encoded path", call(http.MethodGet, "/v1/magpie/heap/%73tats", remote, host))
	check("HEAD", call(http.MethodHead, sampleHeapPath, remote, host))
	wslKey := WSLKey()
	if wslKey == "" {
		t.Fatal("could not make a private WSL key")
	}
	check("WSL path", call(http.MethodGet, "/"+wslKey+sampleHeapPath, remote, host))
	readOnly := call(http.MethodGet, sampleHeapPath, remote, host, "Authorization", "Bearer "+secrets[1])
	if readOnly.Code != http.StatusForbidden || !strings.Contains(readOnly.Body.String(), ReadOnlyCode) {
		t.Errorf("read-only key: %d %q", readOnly.Code, readOnly.Body.String())
	}
}
