package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func fieldlessConnectedCLIAgent(t *testing.T) *agent.Agent {
	t.Helper()
	fieldlessCLIAgent(t)
	host := extension.Default()
	installed, err := host.List()
	if err != nil || len(installed) != 1 {
		t.Fatalf("isolated SDK catalog: %+v %v", installed, err)
	}
	source := t.TempDir()
	manifest := installed[0].Manifest
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-mod=readonly", "-p=1",
		"-o", filepath.Join(source, manifest.Entry), "../extension/testdata/actionworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build isolated existing SDK fixture: %v\n%s", err, output)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	resolved, err := agent.Find("fieldless-sdk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	if state := resolved.Native.Read(); state.Provider != "connected" {
		t.Fatalf("fixture never connected: %+v", state)
	}
	return resolved
}

func TestFieldlessSDKAgentCLIDefaultRestoresConnectedAndDisabledOwners(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			resolved := fieldlessConnectedCLIAgent(t)
			if disabled {
				if err := extension.Default().SetEnabled(t.Context(), "fixture.fieldless-cli", false); err != nil {
					t.Fatal(err)
				}
			}
			for index, word := range []string{"default", "DEFAULT"} {
				err := runFieldlessCommand(resolved.ID, word)
				if disabled && index != 0 {
					if err == nil || !strings.Contains(err.Error(), "unknown agent") {
						t.Fatalf("disconnected disabled descriptor remained available: %v", err)
					}
				} else if err != nil {
					t.Fatalf("checked SDK disconnect failed: %v", err)
				}
			}
			body, err := os.ReadFile(resolved.Path)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]string
			if err := json.Unmarshal(body, &values); err != nil || !reflect.DeepEqual(values, map[string]string{"user": "preserved"}) {
				t.Fatalf("default lost user configuration or retained owned credentials: %q %v", body, err)
			}
		})
	}
}

func TestFieldlessSDKAgentCLIDefaultPreservesUnreadableOwnership(t *testing.T) {
	resolved := fieldlessConnectedCLIAgent(t)
	before, err := os.ReadFile(resolved.Path)
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(filepath.Dir(provider.Path()), "sdk-agent-bindings.json")
	broken := []byte("{broken ownership\n")
	if err := os.WriteFile(ledger, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runFieldlessCommand(resolved.ID, "default"); err == nil ||
		strings.Contains(err.Error(), "panicked") || strings.Contains(err.Error(), "no configurable fields") {
		t.Fatalf("unreadable ownership did not reach the checked disconnect owner: %v", err)
	}
	after, err := os.ReadFile(resolved.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed disconnect changed agent files: %v", err)
	}
	after, err = os.ReadFile(ledger)
	if err != nil || !bytes.Equal(broken, after) {
		t.Fatalf("failed disconnect overwrote unknown ownership: %v", err)
	}
}
