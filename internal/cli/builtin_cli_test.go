package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/testenv"
)

func TestBuiltinListReportsRuntimeState(t *testing.T) {
	const child = "MAGPIE_TEST_BUILTIN_LIST"
	if os.Getenv(child) == "1" {
		pluginrt.Register(pluginrt.Manifest{ID: "cli-active", Name: "Active feature"})
		pluginrt.Register(pluginrt.Manifest{ID: "cli-off", Name: "Disabled feature"})
		pluginrt.Register(pluginrt.Manifest{
			ID: "cli-pending", Name: "Waiting feature", Requires: []string{"missing-service"},
		})
		pluginrt.Register(pluginrt.Manifest{
			ID: "cli-failed", Name: "Failed feature",
			Apply: func(*pluginrt.Ctx) error { return errors.New("fixture initialization failed") },
		})
		if err := pluginrt.Set("cli-off", false); err != nil {
			t.Fatal(err)
		}
		if err := pluginCmd([]string{"plugin", "builtin", "list"}); err != nil {
			t.Fatal(err)
		}
		pluginrt.Running().Stop()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestBuiltinListReportsRuntimeState$")
	command.Env = append(slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, testenv.Marker+"=")
	}), child+"=1", "NO_COLOR=1")
	out, err := command.Output()
	if err != nil {
		t.Fatalf("builtin list: %v\n%s", err, out)
	}
	lines := make(map[string]string)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			lines[fields[1]] = strings.Join(fields[2:], " ")
		}
	}
	for id, want := range map[string]string{
		"cli-active":  "on Active feature",
		"cli-off":     "off Disabled feature",
		"cli-pending": "pending Waiting feature",
		"cli-failed":  "failed Failed feature",
	} {
		if !strings.HasPrefix(lines[id], want) {
			t.Errorf("%s: got %q, want prefix %q", id, lines[id], want)
		}
	}
	for id, why := range map[string]string{
		"cli-pending": "waiting for missing-service",
		"cli-failed":  "fixture initialization failed",
	} {
		if !strings.Contains(lines[id], why) {
			t.Errorf("%s: %q does not explain %q", id, lines[id], why)
		}
	}
}
