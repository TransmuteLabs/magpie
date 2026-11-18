//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var nativeExampleDistribution = flag.String("native-test-distribution", "", "parent-owned native example distribution")
var nativeExampleRoot = flag.String("native-test-root", "", "parent-owned native example catalog")

func nativeBuildExample(t *testing.T) string {
	t.Helper()
	distribution := t.TempDir()
	filename := "plugin.so"
	switch runtime.GOOS {
	case "darwin":
		filename = "plugin.dylib"
	case "windows":
		filename = "plugin.dll"
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "examples", "extensions", "echo-native", sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Entry = filename
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distribution, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, "go", "build", "-trimpath", "-buildmode=c-shared", "-o",
		filepath.Join(distribution, filename), "./examples/extensions/echo-native")
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build public native example: %v\n%s", err, output)
	}
	return distribution
}

func nativeToolContent(t *testing.T, result sdk.ToolResult, value any) {
	t.Helper()
	var content []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result.Content, &content); err != nil || len(content) != 1 {
		t.Fatalf("native result content=%s, err=%v", result.Content, err)
	}
	if err := json.Unmarshal([]byte(content[0].Text), value); err != nil {
		t.Fatalf("native result text=%q, err=%v", content[0].Text, err)
	}
}

func TestNativeSharedLibraryLifecycle(t *testing.T) {
	if *nativeExampleDistribution != "" {
		if *nativeExampleRoot == "" {
			t.Fatal("native child fixture requires a parent-owned catalog")
		}
		nativeSharedLibraryLifecycle(t, *nativeExampleDistribution, *nativeExampleRoot)
		return
	}
	distribution := nativeBuildExample(t)
	root := filepath.Join(t.TempDir(), "catalog")
	if runtime.GOOS == "windows" {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeSharedLibraryLifecycle$", "-test.v",
			"-native-test-distribution="+distribution, "-native-test-root="+root)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("native Windows host fixture: %v\n%s", err, output)
		} else {
			t.Log(string(output))
		}
		return
	}
	nativeSharedLibraryLifecycle(t, distribution, root)
}

func nativeSharedLibraryLifecycle(t *testing.T, distribution, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	manager := NewManager(root)
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	entry, err := manager.Install(ctx, distribution)
	if err != nil {
		t.Fatal(err)
	}
	var pidResult sdk.ToolResult
	if err := manager.Invoke(ctx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "pid", Arguments: json.RawMessage(`{"text":"native"}`)}, &pidResult, nil); err != nil {
		t.Fatal(err)
	}
	var identity struct {
		PID  int    `json:"pid"`
		Text string `json:"text"`
	}
	nativeToolContent(t, pidResult, &identity)
	if identity.PID != os.Getpid() || identity.Text != "native" {
		t.Fatalf("module ran outside its host process: %+v, host=%d", identity, os.Getpid())
	}
	t.Logf("native module PID=%d equals host PID=%d", identity.PID, os.Getpid())
	var group sync.WaitGroup
	results := make(chan error, 8)
	for index := range 8 {
		group.Go(func() {
			var result sdk.ToolResult
			arguments, err := json.Marshal(map[string]string{"text": fmt.Sprintf("concurrent-%d", index)})
			if err == nil {
				err = manager.Invoke(ctx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
					sdk.ToolRequest{ID: "pid", Arguments: arguments}, &result, nil)
			}
			results <- err
		})
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent native call: %v", err)
		}
	}
	var stateResult sdk.ToolResult
	if err := manager.Invoke(ctx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "state", Arguments: json.RawMessage(`{"key":"native-host","value":{"kept":true}}`)}, &stateResult, nil); err != nil {
		t.Fatalf("native scoped host callback: %v", err)
	}
	stored, err := manager.Store().StateGet(sdk.Scope{Plugin: entry.Manifest.ID}, "native-host")
	if err != nil || !bytes.Equal(stored.Value, []byte(`{"kept":true}`)) {
		t.Fatalf("native host callback did not reach scoped state: %+v, %v", stored, err)
	}
	text := strings.Repeat("x", sdk.MaxChunkBytes+17)
	arguments, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	streamDone := make(chan error, 1)
	var streamed []byte
	go func() {
		var result sdk.ToolResult
		err := manager.Invoke(ctx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "stream", Arguments: arguments}, &result, func(event Event) error {
				if event.Name == "data" {
					select {
					case <-started:
					default:
						close(started)
						<-release
					}
					var chunk []byte
					if err := json.Unmarshal(event.Data, &chunk); err != nil {
						return err
					}
					streamed = append(streamed, chunk...)
				}
				return nil
			})
		streamDone <- err
	}()
	nativeTestReceive(t, started)
	select {
	case err := <-streamDone:
		t.Fatalf("native stream did not wait for consumer ACK: %v", err)
	default:
	}
	replacement, err := manager.Install(ctx, distribution)
	if err != nil {
		t.Fatalf("replace native module during a stream: %v", err)
	}
	if replacement.Revision == entry.Revision || replacement.Directory == entry.Directory {
		t.Fatal("native replacement reused the active immutable generation")
	}
	releaseOnce.Do(func() { close(release) })
	if err := nativeTestReceive(t, streamDone); err != nil || string(streamed) != text {
		t.Fatalf("native replacement cut the old stream: %v, bytes=%d", err, len(streamed))
	}
	waitCtx, stopWait := context.WithCancel(ctx)
	defer stopWait()
	accepted := make(chan struct{})
	waitDone := make(chan error, 1)
	go func() {
		_, err := manager.Call(waitCtx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "wait", Arguments: json.RawMessage(`{}`)}, func(event Event) error {
				if event.Name == "accepted" {
					close(accepted)
				}
				return nil
			})
		waitDone <- err
	}()
	nativeTestReceive(t, accepted)
	stopWait()
	if err := nativeTestReceive(t, waitDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("native cancellation did not end its call: %v", err)
	}
	if err := manager.SetEnabled(ctx, entry.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if lease, err := manager.Acquire(ctx, entry.Manifest.ID); err == nil {
		lease.Close()
		t.Fatal("disabled native module admitted new calls")
	}
	if err := manager.SetEnabled(ctx, entry.Manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(ctx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "pid", Arguments: json.RawMessage(`{"text":"reopened"}`)}, &pidResult, nil); err != nil {
		t.Fatalf("native session could not reopen after disable: %v", err)
	}
	nativeToolContent(t, pidResult, &identity)
	if identity.PID != os.Getpid() || identity.Text != "reopened" {
		t.Fatalf("native reopen lost its host identity: %+v", identity)
	}
}
