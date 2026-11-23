//go:build linux || darwin

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	stats "github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var serveForcedSDK = flag.Bool("serve-forced-sdk", false, "exercise SIGTERM deadline with an installed SDK worker")

func TestServeForcedDrainCancelsSDKStream(t *testing.T) {
	if !*serveForcedSDK {
		t.Skip("enable with -args -serve-forced-sdk")
	}
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	usagePath := filepath.Join(config, "magpie", "usage.jsonl")
	if stats.Path() != usagePath {
		t.Fatalf("Usage path escaped the isolated config: %q", stats.Path())
	}

	const session = "forced-sdk-shutdown"
	const delta = "one SDK delta before SIGTERM"
	type dispatch struct {
		operation   string
		pid, parent int
	}
	var selected atomic.Pointer[dispatch]
	var sends atomic.Int32
	canceled := make(chan struct{}, 1)
	stopVendor := make(chan struct{})
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if sends.Add(1) != 1 {
			t.Error("the accepted SDK request was sent again")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 4097))
		pid, pidErr := strconv.Atoi(request.Header.Get("X-Fixture-Pid"))
		parent, parentErr := strconv.Atoi(request.Header.Get("X-Fixture-Parent-Pid"))
		operation := request.Header.Get("X-Fixture-Operation")
		if err != nil || len(body) > 4096 || !json.Valid(body) || pidErr != nil || parentErr != nil ||
			pid <= 0 || parent <= 0 || pid == parent || operation == "" ||
			request.Method != http.MethodPost || request.URL.Path != "/v1/responses" ||
			gjson.GetBytes(body, "model").String() != "m1" || gjson.GetBytes(body, "input").String() != session ||
			gjson.GetBytes(body, "stream").Type != gjson.True {
			t.Error("unexpected SDK request or worker identity")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		selected.Store(&dispatch{operation: operation, pid: pid, parent: parent})
		writer.Header().Set("Content-Type", "text/event-stream")
		_, err = io.WriteString(writer, "event: response.created\n"+
			"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_forced_sdk\",\"model\":\"m1\",\"status\":\"in_progress\",\"output\":[]}}\n\n"+
			"event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"item_forced_sdk\",\"output_index\":0,\"content_index\":0,\"delta\":"+strconv.Quote(delta)+"}\n\n")
		if err != nil || http.NewResponseController(writer).Flush() != nil {
			t.Error("could not flush the held SDK delta")
			return
		}
		select {
		case <-request.Context().Done():
			canceled <- struct{}{}
		case <-stopVendor:
		}
	}))
	defer close(stopVendor)

	manager, installed, account := installForcedSDKWorker(t, vendor.URL)
	workerPath := filepath.Join(manager.Namespace(), installed.Directory, installed.Manifest.Entry)
	wantWorker, err := os.Stat(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	ownedWorker := func(pid int) bool {
		executable, err := proc.Executable(pid)
		if err != nil {
			return false
		}
		info, err := os.Stat(executable)
		return err == nil && os.SameFile(info, wantWorker)
	}

	ctx, stop := context.WithTimeout(t.Context(), 35*time.Second)
	defer stop()
	command := proc.CommandContext(ctx, os.Args[0], "serve")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "MAGPIE_CLI_TEST_SERVE=1", "MAGPIE_ADDR=127.0.0.1:0",
		"MAGPIE_DRAIN=500ms", "KUBERNETES_SERVICE_HOST=", "MAGPIE_TLS_CERT=", "MAGPIE_TLS_KEY=", "MAGPIE_PROFILE_DIR=")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(exited)
	}()
	defer func() {
		stop()
		<-exited
		if call := selected.Load(); call != nil && call.parent == command.Process.Pid && ownedWorker(call.pid) {
			if err := proc.Kill(call.pid); err != nil && ownedWorker(call.pid) {
				t.Errorf("kill leftover fixture worker: %v", err)
			}
		}
		if t.Failed() {
			t.Logf("serve output:\n%s", output.String())
		}
	}()

	address := waitForcedSDKListener(t, ctx, exited, command.Process.Pid)
	publicID := provider.ExtensionProviderID(account.Plugin, account.Provider)
	payload, err := json.Marshal(map[string]any{"model": publicID + "/m1", "input": session, "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/v1/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+gateway.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(gateway.SessionHeader, session)
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK || !strings.HasPrefix(reply.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("held request returned %s, content type %q", reply.Status, reply.Header.Get("Content-Type"))
	}
	stream := bufio.NewScanner(reply.Body)
	observed := false
	for stream.Scan() {
		event := gjson.Parse(strings.TrimPrefix(stream.Text(), "data: "))
		if event.Get("type").String() == "response.output_text.delta" && event.Get("delta").String() == delta {
			observed = true
			break
		}
	}
	if !observed {
		t.Fatalf("SDK delta never reached the client: %v", stream.Err())
	}
	call := selected.Load()
	if call == nil || call.parent != command.Process.Pid || !ownedWorker(call.pid) {
		t.Fatalf("first delta came from an unexpected worker: %+v", call)
	}
	store := extension.NewStore(manager.Namespace())
	receipt, err := store.OperationContext(ctx, account.Plugin, call.operation)
	if err != nil || receipt.State != "accepted" || receipt.Scope.Provider != account.Provider ||
		receipt.Scope.Account != account.ID || receipt.Scope.Generation != account.Generation {
		t.Fatalf("expected a durable accepted receipt before SIGTERM: %+v, %v", receipt, err)
	}
	select {
	case <-canceled:
		t.Fatal("SDK upstream was canceled before SIGTERM")
	default:
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for stream.Scan() {
		event := gjson.Parse(strings.TrimPrefix(stream.Text(), "data: "))
		switch event.Get("type").String() {
		case "response.completed", "response.output_text.delta":
			t.Errorf("held SDK stream produced an unexpected event after SIGTERM: %s", stream.Text())
		}
	}
	if stream.Err() == nil || ctx.Err() != nil {
		t.Errorf("expected the server to cut the incomplete stream: read %v, client %v", stream.Err(), ctx.Err())
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Error("the SDK upstream did not observe cancellation")
	}
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("serve did not exit within the test bound")
	}
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatalf("expected termination by SIGTERM, got %v", waitErr)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("expected Unix wait status, got %T", exit.Sys())
	}
	switch {
	case status.Signaled() && status.Signal() == syscall.SIGTERM:
		t.Log("serve terminated by SIGTERM")
	case status.Exited() && status.ExitStatus() == 128+int(syscall.SIGTERM):
		t.Log("serve used the numeric SIGTERM exit fallback (143)")
	default:
		t.Fatalf("expected SIGTERM or its numeric fallback, got %v", waitErr)
	}
	if !strings.Contains(output.String(), "gateway: ending after 500ms with requests still in flight") {
		t.Error("serve did not report the positive grace expiring with an active request")
	}
	if err := syscall.Kill(call.pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the SDK worker still exists after serve exits: pid=%d, probe=%v", call.pid, err)
	}
	if _, err := os.Stat(gateway.ListeningPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("listener record remains after serve exits: %v", err)
	}
	if sends.Load() != 1 {
		t.Errorf("the fixture received %d executions, want exactly one", sends.Load())
	}
	after, err := store.OperationContext(ctx, account.Plugin, call.operation)
	if err != nil || after.Scope != receipt.Scope || after.State != "uncertain" {
		t.Errorf("terminal SDK receipt after serve exits: %+v, %v", after, err)
	}
	rows := checkForcedSDKUsage(t, usagePath, publicID, session, account.ID)
	t.Logf("grace-expired SDK request: dispatches=%d terminal receipt=%s canceled Usage rows=%d", sends.Load(), after.State, rows)
}

func checkForcedSDKUsage(t *testing.T, path, publicID, session, account string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		rows++
		var row stats.Record
		if err := json.Unmarshal(lines.Bytes(), &row); err != nil {
			t.Fatalf("invalid isolated Usage row %d: %v", rows, err)
		}
		if row.Provider != publicID || row.Session != session || row.ProviderAccount != account || row.Model != "m1" {
			t.Errorf("misattributed Usage for the cut request: %+v", row)
		}
		if row.Error == "" || !(row.Canceled() || row.Status == http.StatusConflict && row.ErrType == "extension_operation_terminal") {
			t.Errorf("unexpected Usage outcome for the cut request: HTTP %d, type %q, error %q", row.Status, row.ErrType, row.Error)
		}
		if row.Input != 0 || row.Output != 0 || row.CacheRead != 0 || row.CacheWrite != 0 || row.CacheWrite1h != 0 ||
			row.Reasoning != 0 || row.Credits != nil || row.OwnCost != nil || row.ReportedCost != nil {
			t.Errorf("billable Usage for the cut request: %+v", row)
		}
		t.Logf("isolated Usage row %d: HTTP %d, type %q", rows, row.Status, row.ErrType)
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if rows > 1 {
		t.Errorf("the cut request produced %d Usage rows, want at most one", rows)
	}
	return rows
}

func installForcedSDKWorker(t *testing.T, origin string) (*extension.Manager, extension.Installed, extension.Account) {
	t.Helper()
	const fixture = "../gateway/testdata/extensionm09worker"
	directory := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(fixture, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
	defer stop()
	build := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-p=2", "-mod=readonly",
		"-o", filepath.Join(directory, "worker"), fixture)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build SDK worker: %v\n%s", err, output)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	installed, err := manager.Install(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := json.Marshal(map[string]string{"origin": origin, "marker": os.Getenv("FAKE_RUN")})
	if err != nil {
		t.Fatal(err)
	}
	account, err := manager.Store().SaveAccount(extension.Account{
		Plugin: installed.Manifest.ID, Provider: installed.Manifest.Providers[0].ID, ID: "alice", Auth: auth,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return manager, installed, account
}

func waitForcedSDKListener(t *testing.T, ctx context.Context, exited <-chan struct{}, pid int) string {
	t.Helper()
	poll := time.NewTicker(15 * time.Millisecond)
	defer poll.Stop()
	for {
		if state, ok := gateway.ReadListening(); ok && state.Pid == pid && state.Serves == "serve" {
			return state.Addr
		}
		select {
		case <-exited:
			t.Fatal("serve exited without publishing its listener")
		case <-ctx.Done():
			t.Fatal("serve did not publish its listener")
		case <-poll.C:
		}
	}
}
