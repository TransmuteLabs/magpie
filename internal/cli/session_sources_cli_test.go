package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/sessions"
)

func TestSessionSourceCLIReadAndHelpCreateNoState(t *testing.T) {
	home := helpHome(t)
	before := tree(t, home)
	for _, args := range [][]string{nil, {"--json"}, {"--help"}} {
		var out bytes.Buffer
		if err := sessionSourcesTo(&out, args); err != nil {
			t.Fatal(err)
		}
		if len(args) > 0 && args[0] == "--help" {
			if !strings.Contains(out.String(), sessionSourcesUsage) {
				t.Fatal("source help omitted checked binding commands")
			}
		} else {
			var status sessions.SessionSourceStatus
			if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Pending ||
				status.Errors == nil || len(status.Errors) != 0 || status.Bindings == nil || len(status.Bindings) != 0 {
				t.Fatalf("empty source status: %s, %v", out.String(), err)
			}
		}
		if after := tree(t, home); !reflect.DeepEqual(before, after) {
			t.Fatal("source inspection or help created state")
		}
	}
	for _, args := range [][]string{
		{"bind", "module", "source"}, {"unbind", "omp"}, {"--json", "--unknown"},
		{"bind", "module", "source", "--expected", "revision", "extra"},
	} {
		var out bytes.Buffer
		if err := sessionSourcesTo(&out, args); err == nil {
			t.Fatal("unchecked source command accepted", args)
		}
		if after := tree(t, home); !reflect.DeepEqual(before, after) {
			t.Fatal("invalid source command created state", args)
		}
	}
}

func TestSessionSourceCLINativeBindingLifecycle(t *testing.T) {
	distribution := os.Getenv("MAGPIE_SOURCE_TEST_DIST")
	if distribution == "" {
		t.Skip("requires explicitly built synthetic native Source fixture")
	}
	helpHome(t)
	if err := sessions.SetReads(nil, map[string]bool{"fixture-source": true, "omp": true}); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
		sessions.Reset()
	})
	installed, err := manager.Install(t.Context(), distribution)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) error {
		var out bytes.Buffer
		return sessionSourcesTo(&out, args)
	}
	status := func() sessions.SessionSourceStatus {
		t.Helper()
		var out bytes.Buffer
		if err := sessionSourcesTo(&out, []string{"--json"}); err != nil {
			t.Fatalf("source inspection: %v, %s", err, out.String())
		}
		var result sessions.SessionSourceStatus
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if err := invoke("bind", installed.Manifest.ID, "native-omp", "--expected", "stale"); err == nil {
		t.Fatal("stale package revision acquired native history")
	}
	if len(status().Bindings) != 0 {
		t.Fatal("rejected binding left ownership behind")
	}
	if err := invoke("bind", installed.Manifest.ID, "native-omp", "--expected", installed.Revision); err != nil {
		t.Fatal(err)
	}
	bound := status().Bindings
	if len(bound) != 1 || bound[0].Agent != "omp" || bound[0].Identity == "" {
		t.Fatal("checked binding missing from ordinary inspection", bound)
	}
	if err := invoke("unbind", "omp", "--expected", "stale"); err == nil {
		t.Fatal("stale identity released native history ownership")
	}
	if len(status().Bindings) != 1 {
		t.Fatal("stale release erased native binding")
	}
	if err := manager.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := invoke("unbind", "omp", "--expected", bound[0].Identity); err != nil {
		t.Fatal("disabled source could not release its exact ownership", err)
	}
	if len(status().Bindings) != 0 {
		t.Fatal("released native source retained ownership")
	}
}
