package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var (
	m09RuntimeParity = flag.Bool("m09-runtime-parity", false, "build and compare the SDK exec and native Responses fixtures")
	m09HostConfig    = flag.String("m09-host-config", "", "serve a single M09 fixture from a private test configuration")
)

//go:embed testdata/extensionm09worker/magpie-plugin.json
var m09Descriptor []byte

const (
	m09Clients     = 32
	m09CancelTurn  = m09Clients + 1
	m09SiblingTurn = m09CancelTurn + 1
	m09Turns       = m09SiblingTurn + 1
	m09MinOverlap  = 8
	m09DeltaCount  = 32
	m09DeltaBytes  = 16 << 10
)

type m09UpstreamTurn struct {
	Turn                       int
	Operation                  string
	PID, ParentPID, GOMAXPROCS int
	Request                    [32]byte
}

type m09HostSetup struct {
	Package, Ready, Origin, Marker string
}

type m09HostReady struct {
	URL, Model, CallerKey string
	PID, GOMAXPROCS       int
}

type m09WireComparison struct {
	Request, Response [32]byte
}

type m09HostReport struct {
	Completed, Uncertain, BillableRows, CanceledRows int
	CanceledStatus                                   int
	CanceledType                                     string
}

type m09Wave struct {
	mu            sync.Mutex
	turns         [m09Turns]m09UpstreamTurn
	active, peak  int
	overlap, pair chan struct{}
	cancelSeen    chan struct{}
}

func m09Deltas() [m09DeltaCount]string {
	const lineBytes = 64
	var deltas [m09DeltaCount]string
	for index := range deltas {
		var text strings.Builder
		text.Grow(m09DeltaBytes)
		for line := range m09DeltaBytes / lineBytes {
			sample := sha256.Sum256([]byte(fmt.Sprintf("m09:%02d:%03d", index, line)))
			fmt.Fprintf(&text, "%02d:%03d sample %x %x\n", index, line, sample[:12], sample[12:24])
		}
		deltas[index] = text.String()
	}
	return deltas
}

func TestM09SDKRuntimeParity(t *testing.T) {
	if *m09HostConfig != "" {
		serveM09Host(t, *m09HostConfig)
		return
	}
	if !*m09RuntimeParity {
		t.Skip("enable with -args -m09-runtime-parity")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("M09 fixture supports Darwin and Linux native loaders")
	}

	deltas := m09Deltas()
	var frames [m09DeltaCount]string
	for index := range frames {
		if len(deltas[index]) != m09DeltaBytes {
			t.Fatal("incorrect M09 delta size")
		}
		frames[index] = fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"item_m09\",\"output_index\":0,\"content_index\":0,\"sequence_number\":%d,\"delta\":%q}\n\n", index+1, deltas[index])
	}
	var expected [m09Turns]m09WireComparison
	var execReport m09HostReport
	var execHost int
	for _, kind := range []string{"exec", "native"} {
		if !t.Run(kind, func(t *testing.T) {
			wave := &m09Wave{
				overlap: make(chan struct{}), pair: make(chan struct{}), cancelSeen: make(chan struct{}),
			}
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				wave.serve(t, writer, request, frames)
			}))
			directory := buildM09Module(t, kind)
			setup := m09HostSetup{Package: directory, Ready: filepath.Join(t.TempDir(), "ready.json"),
				Origin: vendor.URL, Marker: os.Getenv("FAKE_RUN")}
			responses, host, worker, report := runM09Host(t, setup, wave, deltas)
			if kind == "exec" && (worker.PID == host.PID || worker.ParentPID != host.PID) ||
				kind == "native" && (worker.PID != host.PID || worker.ParentPID != os.Getpid() || host.PID == execHost) {
				t.Fatalf("wrong %s runtime identity: driver %d, host %d, worker %d, worker parent %d",
					kind, os.Getpid(), host.PID, worker.PID, worker.ParentPID)
			}
			t.Logf("M09 identity %s driver=%d host=%d host_procs=%d worker=%d worker_parent=%d worker_procs=%d",
				kind, os.Getpid(), host.PID, host.GOMAXPROCS, worker.PID, worker.ParentPID, worker.GOMAXPROCS)
			t.Logf("M09 workload %s turns=%d clients=1,32 upstream_peak=%d canceled=1 sibling=1",
				kind, len(responses), wave.peak)
			t.Logf("M09 accounting %s completed=%d uncertain=%d billable_rows=%d canceled_rows=%d canceled_status=%d canceled_billing=0",
				kind, report.Completed, report.Uncertain, report.BillableRows, report.CanceledRows, report.CanceledStatus)
			if kind == "exec" {
				expected, execHost, execReport = responses, host.PID, report
				return
			}
			if report != execReport {
				t.Errorf("M09 accounting differs: exec %+v, native %+v", execReport, report)
			}
			upstream, public := sha256.New(), sha256.New()
			for turn, response := range responses {
				if previous := expected[turn]; response != previous {
					t.Errorf("turn %d wire digest differs: upstream %x vs %x, response %x vs %x",
						turn, previous.Request, response.Request, previous.Response, response.Response)
				}
				upstream.Write(response.Request[:])
				public.Write(response.Response[:])
			}
			if !t.Failed() {
				t.Logf("M09 wire parity upstream=%d public=%d upstream_sha256=%x public_sha256=%x",
					len(responses), len(responses), upstream.Sum(nil), public.Sum(nil))
			}
		}) {
			return
		}
	}
}

func (wave *m09Wave) serve(t *testing.T, writer http.ResponseWriter, request *http.Request, frames [m09DeltaCount]string) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 4097))
	turn, turnErr := strconv.Atoi(gjson.GetBytes(body, "metadata.m09_turn").String())
	pid, pidErr := strconv.Atoi(request.Header.Get("X-Fixture-Pid"))
	parent, parentErr := strconv.Atoi(request.Header.Get("X-Fixture-Parent-Pid"))
	processors, processorsErr := strconv.Atoi(request.Header.Get("X-Fixture-Gomaxprocs"))
	operation := request.Header.Get("X-Fixture-Operation")
	if err != nil || len(body) > 4096 || !json.Valid(body) || turnErr != nil || turn < 0 || turn >= m09Turns ||
		pidErr != nil || pid <= 0 || parentErr != nil || parent <= 0 || processorsErr != nil || processors <= 0 || operation == "" ||
		request.Method != http.MethodPost || request.URL.Path != "/v1/responses" ||
		gjson.GetBytes(body, "model").String() != "m1" || gjson.GetBytes(body, "stream").Type != gjson.True ||
		gjson.GetBytes(body, "input").String() != m09Prompt(turn) || request.Header.Get("Authorization") != "" {
		t.Error("M09 vendor received unexpected request shape, identity or route")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	overlap := turn > 0 && turn <= m09Clients
	wave.mu.Lock()
	duplicate := wave.turns[turn].Operation != ""
	if !duplicate {
		wave.turns[turn] = m09UpstreamTurn{
			Turn: turn, Operation: operation, PID: pid, ParentPID: parent, GOMAXPROCS: processors, Request: sha256.Sum256(body),
		}
		if overlap {
			wave.active++
			if wave.active > wave.peak {
				wave.peak = wave.active
				if wave.peak == m09MinOverlap {
					close(wave.overlap)
				}
			}
		}
		if turn >= m09CancelTurn && wave.turns[m09CancelTurn].Operation != "" && wave.turns[m09SiblingTurn].Operation != "" {
			close(wave.pair)
		}
	}
	wave.mu.Unlock()
	if duplicate {
		t.Errorf("M09 turn %d reached the vendor more than once", turn)
		writer.WriteHeader(http.StatusConflict)
		return
	}
	if overlap {
		defer func() {
			wave.mu.Lock()
			wave.active--
			wave.mu.Unlock()
		}()
	}
	var gate <-chan struct{}
	switch {
	case overlap:
		gate = wave.overlap
	case turn == m09CancelTurn:
		gate = wave.pair
	case turn == m09SiblingTurn:
		gate = wave.cancelSeen
	}
	ctx, stop := context.WithTimeout(request.Context(), 15*time.Second)
	defer stop()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			t.Errorf("M09 turn %d did not reach its upstream overlap/cancel gate: %v", turn, ctx.Err())
			return
		}
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	controller := http.NewResponseController(writer)
	responseID := m09ResponseID(turn)
	if _, err := fmt.Fprintf(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":%q,\"model\":\"m1\",\"status\":\"in_progress\",\"output\":[]}}\n\n", responseID); err != nil {
		t.Errorf("M09 upstream turn %d start: %v", turn, err)
		return
	}
	for _, frame := range frames {
		_, writeErr := io.WriteString(writer, frame)
		if err := errors.Join(writeErr, controller.Flush()); err != nil {
			t.Errorf("M09 upstream turn %d delta: %v", turn, err)
			return
		}
		if turn == m09CancelTurn {
			<-ctx.Done()
			if request.Context().Err() != context.Canceled {
				t.Error("M09 public cancellation did not reach the upstream request")
				return
			}
			close(wave.cancelSeen)
			return
		}
	}
	if _, err := fmt.Fprintf(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":33,\"response\":{\"id\":%q,\"model\":\"m1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"item_m09\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"m09-terminal\"}]}],\"usage\":{\"input_tokens\":16,\"output_tokens\":32,\"total_tokens\":48}}}\n\n", responseID); err != nil {
		t.Errorf("M09 upstream turn %d terminal: %v", turn, err)
	}
}

func buildM09Module(t *testing.T, kind string) string {
	t.Helper()
	var manifest sdk.Manifest
	if err := json.Unmarshal(m09Descriptor, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Runtime = kind
	directory := t.TempDir()
	arguments := []string{"build", "-p=2", "-trimpath", "-mod=readonly"}
	if kind == "native" {
		manifest.Entry = "worker.so"
		if runtime.GOOS == "darwin" {
			manifest.Entry = "worker.dylib"
		}
		arguments = append(arguments, "-buildmode=c-shared")
	}
	arguments = append(arguments, "-o", filepath.Join(directory, manifest.Entry), "./testdata/extensionm09worker")
	ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
	defer stop()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), arguments...)
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s M09 module: %v\n%s", kind, err, output)
	}
	if err := os.Remove(filepath.Join(directory, "worker.h")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	writeM09JSON(t, filepath.Join(directory, sdk.ManifestFile), manifest)
	return directory
}

func runM09Host(t *testing.T, setup m09HostSetup, wave *m09Wave, deltas [m09DeltaCount]string) (
	[m09Turns]m09WireComparison, m09HostReady, m09UpstreamTurn, m09HostReport,
) {
	t.Helper()
	config := filepath.Join(filepath.Dir(setup.Ready), "config.json")
	writeM09JSON(t, config, setup)
	ctx, stop := context.WithTimeout(t.Context(), 60*time.Second)
	defer stop()
	command := proc.Command(os.Args[0], "-test.run=^TestM09SDKRuntimeParity$", "-test.v", "-m09-host-config="+config)
	log, err := os.Create(filepath.Join(filepath.Dir(setup.Ready), "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	tree, err := proc.StartTree(command)
	if err != nil {
		t.Fatal(err)
	}
	stopTree := context.AfterFunc(ctx, tree.Kill)
	defer stopTree()
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = tree.Wait()
		close(exited)
	}()
	defer func() {
		stop()
		<-exited
		if waitErr != nil || t.Failed() {
			output, _ := os.ReadFile(log.Name())
			t.Logf("M09 host exit %v:\n%s", waitErr, output)
		}
	}()

	ready := waitM09Host(t, ctx, exited, setup.Ready)
	if ready.PID != command.Process.Pid || ready.GOMAXPROCS <= 0 {
		t.Fatalf("wrong host identity: launched %d, ready process %d with GOMAXPROCS=%d",
			command.Process.Pid, ready.PID, ready.GOMAXPROCS)
	}
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var result [m09Turns]m09WireComparison
	result[0].Response, err = readM09Turn(ctx, client, ready, 0, deltas)
	if err != nil {
		t.Fatal(err)
	}
	var pending sync.WaitGroup
	var failures [m09Turns]error
	for turn := 1; turn <= m09Clients; turn++ {
		pending.Go(func() {
			result[turn].Response, failures[turn] = readM09Turn(ctx, client, ready, turn, deltas)
		})
	}
	pending.Wait()
	for turn := m09CancelTurn; turn <= m09SiblingTurn; turn++ {
		pending.Go(func() {
			result[turn].Response, failures[turn] = readM09Turn(ctx, client, ready, turn, deltas)
		})
	}
	pending.Wait()
	for turn, err := range failures {
		if err != nil {
			t.Errorf("M09 turn %d: %v", turn, err)
		}
	}
	select {
	case <-wave.cancelSeen:
	default:
		t.Error("M09 vendor did not acknowledge client cancellation")
	}
	wave.mu.Lock()
	observed, peak := wave.turns, wave.peak
	wave.mu.Unlock()
	if peak < m09MinOverlap {
		t.Errorf("M09 32-client wave reached %d in-flight upstream requests, want at least %d", peak, m09MinOverlap)
	}
	worker := observed[0]
	operations := make(map[string]bool, len(observed))
	for turn, record := range observed {
		if record.Operation == "" || operations[record.Operation] || record.PID != worker.PID ||
			record.ParentPID != worker.ParentPID || record.GOMAXPROCS != worker.GOMAXPROCS {
			t.Errorf("turn %d did not reach the same M09 worker with a unique operation", turn)
		}
		operations[record.Operation] = true
		result[turn].Request = record.Request
	}
	if t.Failed() {
		t.FailNow()
	}
	if err := json.NewEncoder(stdin).Encode(observed); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	<-exited
	if waitErr != nil {
		t.Fatalf("M09 host receipt or Usage check failed: %v", waitErr)
	}
	var report m09HostReport
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(setup.Ready), "accounting.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil || report.Completed != m09Turns-1 ||
		report.Uncertain != 1 || report.BillableRows != m09Turns-1 || report.CanceledRows < 0 || report.CanceledRows > 1 {
		t.Fatalf("invalid M09 accounting report: %+v, %v", report, err)
	}
	return result, ready, worker, report
}

func waitM09Host(t *testing.T, ctx context.Context, exited <-chan struct{}, path string) m09HostReady {
	t.Helper()
	checks := time.NewTicker(15 * time.Millisecond)
	defer checks.Stop()
	for {
		var ready m09HostReady
		raw, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(raw, &ready) == nil {
			return ready
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-exited:
			t.Fatal("M09 host exited before opening its listener")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-checks.C:
		}
	}
}

func serveM09Host(t *testing.T, config string) {
	t.Helper()
	var setup m09HostSetup
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &setup); err != nil {
		t.Fatal(err)
	}
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_ADDR", "")
	manager := extensionhost.Default()
	t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
	installed, err := manager.Install(t.Context(), setup.Package)
	if err != nil {
		t.Fatal(err)
	}
	module, relay := installed.Manifest.ID, installed.Manifest.Providers[0].ID
	auth, err := json.Marshal(map[string]string{"origin": setup.Origin, "marker": setup.Marker})
	if err != nil {
		t.Fatal(err)
	}
	account, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: module, Provider: relay, ID: "alice", Auth: auth}, 0)
	if err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	id := provider.ExtensionProviderID(module, relay)
	keys, secrets := newCaller(t, "M09 fixture")
	server := New()
	t.Cleanup(server.idle.stop)
	front := server.Front()
	finished := make(chan int, m09Turns)
	public := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		turn, err := strconv.Atoi(request.Header.Get("X-Test-M09-Turn"))
		if err != nil || turn < 0 || turn >= m09Turns {
			t.Error("M09 gateway received an unexpected public request")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		request.Header.Del("X-Test-M09-Turn")
		request.RemoteAddr = "192.168.1.9:5000"
		front.ServeHTTP(writer, request)
		select {
		case finished <- turn:
		default:
			t.Error("M09 gateway received extra public requests")
		}
	}))
	t.Cleanup(public.Close)
	writeM09JSON(t, setup.Ready, m09HostReady{
		URL: public.URL, Model: id + "/m1", CallerKey: secrets[0], PID: os.Getpid(), GOMAXPROCS: runtime.GOMAXPROCS(0),
	})

	var observed []m09UpstreamTurn
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 32<<10)).Decode(&observed); err != nil {
		t.Fatal(err)
	}
	if len(observed) != m09Turns {
		t.Fatalf("M09 host received %d proof records, want %d", len(observed), m09Turns)
	}
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	var ended [m09Turns]bool
	for range m09Turns {
		select {
		case turn := <-finished:
			if ended[turn] {
				t.Fatalf("M09 public turn %d finished twice", turn)
			}
			ended[turn] = true
		case <-ctx.Done():
			t.Fatal("M09 public handlers did not finish before journal and Usage checks")
		}
	}
	for index, turn := range observed {
		state := "completed"
		if index == m09CancelTurn {
			state = "uncertain"
		}
		receipt, err := manager.Store().OperationContext(t.Context(), module, turn.Operation)
		if err != nil || turn.Turn != index || receipt.State != state || receipt.Scope.Plugin != module ||
			receipt.Scope.Provider != relay || receipt.Scope.Operation != turn.Operation ||
			receipt.Scope.Account != account.ID || receipt.Scope.Generation != account.Generation {
			t.Errorf("M09 turn %d has wrong receipt: %+v, %v", index, receipt, err)
		}
	}
	counts := make(map[string]int, len(observed))
	for turn := range len(observed) {
		counts[m09ResponseID(turn)] = 0
	}
	report := m09HostReport{Completed: m09Turns - 1, Uncertain: 1, BillableRows: m09Turns - 1}
	usage.Visit(time.Time{}, func(row usage.Record) {
		if row.Provider != id && row.CallerKeyID != keys[0].ID {
			return
		}
		if _, expected := counts[row.Session]; !expected {
			t.Errorf("unexpected M09 Usage record for session %q", row.Session)
			return
		}
		counts[row.Session]++
		if row.CallerKeyID != keys[0].ID || row.Provider != id || row.ProviderAccount != account.ID || row.Model != "m1" {
			t.Errorf("M09 Usage for %q has wrong caller/account/model: %q/%q/%q",
				row.Session, row.CallerKeyID, row.ProviderAccount, row.Model)
		}
		if row.Session == m09ResponseID(m09CancelTurn) {
			report.CanceledStatus, report.CanceledType = row.Status, row.ErrType
			if row.Input != 0 || row.Output != 0 || row.CacheRead != 0 || row.CacheWrite != 0 || row.Error == "" {
				t.Errorf("canceled M09 Usage has success or billing: %d/%d/%d/%d, HTTP %d, error %q",
					row.Input, row.Output, row.CacheRead, row.CacheWrite, row.Status, row.Error)
			}
		} else if row.ResponseID != row.Session || row.Input != 16 || row.Output != 32 ||
			row.Status != http.StatusOK || row.Error != "" {
			t.Errorf("M09 Usage for %q: response %q, tokens %d/%d, HTTP %d, error %q",
				row.Session, row.ResponseID, row.Input, row.Output, row.Status, row.Error)
		}
	})
	report.CanceledRows = counts[m09ResponseID(m09CancelTurn)]
	for session, count := range counts {
		if session == m09ResponseID(m09CancelTurn) && count <= 1 {
			continue
		}
		if count != 1 {
			t.Errorf("M09 session %q has %d Usage rows, want 1", session, count)
		}
	}
	if !t.Failed() {
		writeM09JSON(t, filepath.Join(filepath.Dir(setup.Ready), "accounting.json"), report)
	}
}

func writeM09JSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func m09ResponseID(turn int) string { return fmt.Sprintf("resp_m09_%03d", turn) }

func m09Prompt(turn int) string { return fmt.Sprintf("M09 fixture turn %d", turn) }

func readM09Turn(ctx context.Context, client *http.Client, ready m09HostReady, turn int, expectedDeltas [m09DeltaCount]string) ([32]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"input":%q,"metadata":{"m09_turn":%q}}`, ready.Model, m09Prompt(turn), strconv.Itoa(turn))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ready.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		return [32]byte{}, err
	}
	request.Header.Set("Authorization", "Bearer "+ready.CallerKey)
	request.Header.Set(SessionHeader, m09ResponseID(turn))
	request.Header.Set("X-Test-M09-Turn", strconv.Itoa(turn))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return [32]byte{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		preview, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return [32]byte{}, fmt.Errorf("HTTP %d returned %q", response.StatusCode, preview)
	}
	digest := sha256.New()
	scan := bufio.NewScanner(io.TeeReader(response.Body, digest))
	started, completed, deltas := false, false, 0
	var name string
	var payload []byte
	for scan.Scan() {
		line := scan.Bytes()
		switch {
		case bytes.HasPrefix(line, []byte("event: ")) && name == "" && payload == nil:
			name = string(line[len("event: "):])
		case bytes.HasPrefix(line, []byte("data: ")) && name != "" && payload == nil:
			payload = bytes.Clone(line[len("data: "):])
		case len(line) == 0 && name != "" && payload != nil:
		case len(line) == 0 && name == "" && payload == nil:
			continue
		default:
			return [32]byte{}, errors.New("unexpected M09 SSE line or boundary")
		}
		if len(line) != 0 {
			continue
		}
		if !json.Valid(payload) || gjson.GetBytes(payload, "type").String() != name {
			return [32]byte{}, errors.New("M09 event name and JSON type disagree")
		}
		switch name {
		case "response.created":
			id := gjson.GetBytes(payload, "response.id").String()
			model := gjson.GetBytes(payload, "response.model").String()
			status := gjson.GetBytes(payload, "response.status").String()
			sequence := gjson.GetBytes(payload, "sequence_number").Raw
			if started || id != m09ResponseID(turn) || model != ready.Model || status != "in_progress" || sequence != "0" {
				return [32]byte{}, fmt.Errorf("wrong M09 response start: started=%t id=%q model=%q status=%q sequence=%q, want id=%q model=%q",
					started, id, model, status, sequence, m09ResponseID(turn), ready.Model)
			}
			started = true
		case "response.output_text.delta":
			deltas++
			if !started || completed || deltas > m09DeltaCount ||
				gjson.GetBytes(payload, "delta").String() != expectedDeltas[deltas-1] ||
				gjson.GetBytes(payload, "item_id").String() != "item_m09" ||
				gjson.GetBytes(payload, "output_index").Raw != "0" || gjson.GetBytes(payload, "content_index").Raw != "0" ||
				gjson.GetBytes(payload, "sequence_number").Int() != int64(deltas) {
				return [32]byte{}, fmt.Errorf("wrong M09 delta %d", deltas)
			}
			if turn == m09CancelTurn {
				cancel()
				return [32]byte(digest.Sum(nil)), nil
			}
		case "response.completed":
			model := gjson.GetBytes(payload, "response.model").String()
			if model != ready.Model {
				return [32]byte{}, fmt.Errorf("M09 terminal model %q, want public model %q", model, ready.Model)
			}
			if !started || completed || deltas != m09DeltaCount || gjson.GetBytes(payload, "response.id").String() != m09ResponseID(turn) ||
				gjson.GetBytes(payload, "response.status").String() != "completed" ||
				gjson.GetBytes(payload, "sequence_number").Int() != m09DeltaCount+1 ||
				gjson.GetBytes(payload, "response.usage.input_tokens").Int() != 16 ||
				gjson.GetBytes(payload, "response.usage.output_tokens").Int() != 32 ||
				gjson.GetBytes(payload, "response.usage.total_tokens").Int() != 48 ||
				gjson.GetBytes(payload, "response.output.0.id").String() != "item_m09" ||
				gjson.GetBytes(payload, "response.output.0.status").String() != "completed" ||
				gjson.GetBytes(payload, "response.output.0.content.0.type").String() != "output_text" ||
				gjson.GetBytes(payload, "response.output.0.content.0.text").String() != "m09-terminal" {
				return [32]byte{}, errors.New("wrong M09 response terminal")
			}
			completed = true
		default:
			return [32]byte{}, errors.New("unexpected M09 response event")
		}
		name, payload = "", nil
	}
	if err := scan.Err(); err != nil {
		return [32]byte{}, err
	}
	if !completed || name != "" || payload != nil {
		return [32]byte{}, errors.New("M09 response ended before completion")
	}
	return [32]byte(digest.Sum(nil)), nil
}
