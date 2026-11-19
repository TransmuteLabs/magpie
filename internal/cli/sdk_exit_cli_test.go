package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const sdkExitEnv = "MAGPIE_CLI_TEST_SDK_EXIT"

func sdkExitChild(role string) int {
	if os.Getenv(testenv.Marker) == "" || len(os.Args) != 3 {
		return 2
	}
	testenv.Offline()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	marker, _ := json.Marshal(os.Args[2])
	start := func() error {
		_, err := extension.Default().Call(ctx, "exit-fixture", sdk.MethodModuleCall, sdk.Scope{},
			sdk.ModuleCallRequest{Action: "mark", Input: marker}, nil)
		return err
	}
	if role == "before-first-use" {
		stopSDKComponents()
	}
	if role == "signal" {
		endProbesOnSignal(nil)
	}
	if role != "before-first-use" {
		if err := start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 4
		}
	}
	if role == "after-stop" {
		stopSDKComponents()
	}
	if role == "before-first-use" || role == "after-stop" {
		if err := start(); err == nil {
			fmt.Fprintln(os.Stderr, "SDK admitted a new call during process shutdown")
			return 7
		}
		return 0
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	switch role {
	case "signal":
		process, err := os.FindProcess(os.Getpid())
		if err == nil {
			err = process.Signal(syscall.SIGTERM)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 5
		}
		select {}
	case "success":
		os.Args = []string{"magpie", "version"}
	case "error":
		os.Args = []string{"magpie", "no-such-command-sdk-exit"}
	}
	Main("0.1.9999")
	return 6
}

func sdkExitManifest() sdk.Manifest {
	return sdk.Manifest{
		Schema: 1, ID: "exit-fixture", Version: "1.0.0", Runtime: "exec",
		Entry: "worker",
		Contract: sdk.Contract{
			Version:      sdk.APIVersion{Major: 1, Minor: 1},
			Capabilities: []sdk.Capability{sdk.ModuleRPC},
		},
		Actions: []sdk.ActionDescriptor{{ID: "mark", Effect: "read", InputSchema: json.RawMessage(`{"type":"string"}`)}},
	}
}

func TestCLIExitStopsSDKComponents(t *testing.T) {
	distribution := t.TempDir()
	manifest := sdkExitManifest()
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(distribution, manifest.Entry), "./testdata/sdkexit")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build SDK worker: %v\n%s", err, output)
	}
	cliHome(t)
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distribution, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	if _, err := manager.Install(t.Context(), distribution); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"success", "error", "signal", "before-first-use", "after-stop"} {
		t.Run(role, func(t *testing.T) {
			if role == "signal" && runtime.GOOS == "windows" {
				t.Skip("SIGTERM is unavailable")
			}
			marker := filepath.Join(t.TempDir(), "disposition")
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^$", marker)
			command.Env = append(os.Environ(), sdkExitEnv+"="+role)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("CLI did not finish in time: %v\n%s", ctx.Err(), output)
			}
			switch role {
			case "success", "before-first-use", "after-stop":
				if err != nil {
					t.Fatalf("CLI failed: %v\n%s", err, output)
				}
			case "error", "signal":
				var exit *exec.ExitError
				if !errors.As(err, &exit) || role == "error" && exit.ExitCode() != 1 {
					t.Fatalf("unexpected CLI exit: %v\n%s", err, output)
				}
				if role == "signal" && exit.ExitCode() != 128+int(syscall.SIGTERM) {
					status, ok := exit.Sys().(syscall.WaitStatus)
					if !ok || status.Signal() != syscall.SIGTERM {
						t.Fatalf("CLI did not exit on SIGTERM: %v\n%s", err, output)
					}
				}
			}
			if role == "before-first-use" {
				return
			}
			if result, err := os.ReadFile(marker); err != nil || string(result) != "stopped" {
				t.Fatalf("CLI exited without stopping its SDK component: %q, %v\n%s", result, err, output)
			}
		})
	}
}
