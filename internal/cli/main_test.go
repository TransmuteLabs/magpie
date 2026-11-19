package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/pprof"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

// The tests run in a home of their own, never the user's real files, and
// off the network (testenv.Offline): the plugin tests start plugin hosts,
// and a host started in a home without a models.dev catalog fetched the
// real one for up to 30s (#1922).
//
// With MAGPIE_CLI_TEST_SERVE set, inside a test's sandbox, the test binary
// is `magpie serve` of a released version (serve_update_test.go).
func TestMain(m *testing.M) {
	if os.Getenv("MAGPIE_CLI_TEST_SERVE") != "" {
		if os.Getenv(testenv.Marker) == "" {
			os.Exit(2)
		}
		testenv.Offline()
		replace := serveReexec
		serveReexec = func(exe string, args, env []string) error {
			if err := pprof.StartCPUProfile(io.Discard); err != nil {
				return fmt.Errorf("CPU recorder still active before exec: %w", err)
			}
			pprof.StopCPUProfile()
			return replace(exe, args, env)
		}
		Main("0.1.9999")
	}
	// With MAGPIE_CLI_TEST_HOST_EXIT set, it starts a plugin host and
	// ends as magpie does (plugin_host_exit_cli_test.go).
	if how := os.Getenv(hostExitEnv); how != "" {
		if os.Getenv(testenv.Marker) == "" {
			os.Exit(2)
		}
		testenv.Offline()
		hostThenExit(how)
	}
	if role := os.Getenv(sdkExitEnv); role != "" {
		os.Exit(sdkExitChild(role))
	}
	os.Exit(testenv.RunIn(m, func(string) { testenv.Offline() }))
}

// A host start's models.dev fetch is refused at once, never sent (#1922).
func TestStaysOffTheNetwork(t *testing.T) {
	resp, err := http.Get("https://models.dev/api.json")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("models.dev answered %s: the package's tests reach the network", resp.Status)
	}
	if !strings.Contains(err.Error(), "tests stay off the network") {
		t.Fatalf("models.dev: %v, not testenv's refusal", err)
	}
}
