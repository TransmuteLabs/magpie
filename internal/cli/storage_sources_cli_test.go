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

type storageCLIListing struct {
	Roots    []sessions.StorageSourceRoot    `json:"roots"`
	Consents []sessions.StorageSourceConsent `json:"consents"`
}

func TestStorageSourceCLIListCreatesNoState(test *testing.T) {
	home := helpHome(test)
	before := tree(test, home)
	for _, arguments := range [][]string{{"list"}, {"list", "--json"}} {
		var output bytes.Buffer
		if err := sessionSourcesTo(&output, arguments); err != nil {
			test.Fatal(err)
		}
		var listing storageCLIListing
		if err := json.Unmarshal(output.Bytes(), &listing); err != nil ||
			listing.Roots == nil || len(listing.Roots) != 0 ||
			listing.Consents == nil || len(listing.Consents) != 0 {
			test.Fatalf("empty storage listing: %s, %v", output.String(), err)
		}
		if after := tree(test, home); !reflect.DeepEqual(before, after) {
			test.Fatal("storage source listing created state")
		}
	}
	for _, arguments := range [][]string{
		{"list", "--unknown"},
		{"bind", "module", "source", "--root", "data"},
		{"bind", "module", "source", "--root", "data", "--root-ref", "ref", "--expected"},
		{"bind", "module", "source", "--root", "data", "--root-ref", "ref", "--expected", "revision", "extra"},
	} {
		var output bytes.Buffer
		if err := sessionSourcesTo(&output, arguments); err == nil {
			test.Fatal("unchecked storage selection accepted", arguments)
		}
		if after := tree(test, home); !reflect.DeepEqual(before, after) {
			test.Fatal("invalid storage selection created state", arguments)
		}
	}
}

func TestStorageSourceCLIRootBindingLifecycle(test *testing.T) {
	distribution := os.Getenv("MAGPIE_STORAGE_TEST_DIST")
	if distribution == "" {
		test.Skip("requires the explicitly built OpenCode native storage package")
	}
	helpHome(test)
	if err := sessions.SetReads(nil, map[string]bool{"opencode": false}); err != nil {
		test.Fatal(err)
	}
	if err := os.MkdirAll(sessions.OpenCodeDir(), 0o700); err != nil {
		test.Fatal(err)
	}
	manager := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			test.Error(err)
		}
		sessions.Reset()
	})
	installed, err := manager.Install(test.Context(), distribution)
	if err != nil {
		test.Fatal(err)
	}
	list := func() storageCLIListing {
		test.Helper()
		var output bytes.Buffer
		if err := sessionSourcesTo(&output, []string{"list", "--json"}); err != nil {
			test.Fatal(err)
		}
		var listing storageCLIListing
		if err := json.Unmarshal(output.Bytes(), &listing); err != nil {
			test.Fatal(err)
		}
		if strings.Contains(output.String(), sessions.OpenCodeDir()) {
			test.Fatal("storage listing exposed a filesystem root")
		}
		return listing
	}
	initial := list()
	if len(initial.Roots) != 1 || len(initial.Consents) != 0 ||
		initial.Roots[0].Revision != installed.Revision || len(initial.Roots[0].RootRef) != 64 {
		test.Fatal("installed storage root missing from CLI listing", initial)
	}
	root := initial.Roots[0]
	bind := func(slot, reference, revision string) error {
		var output bytes.Buffer
		return sessionSourcesTo(&output, []string{"bind", root.Plugin, root.Source,
			"--root", slot, "--root-ref", reference, "--expected", revision})
	}
	for _, selection := range [][3]string{
		{root.RootSlot, root.RootRef, "stale"},
		{root.RootSlot, strings.Repeat("0", 64), root.Revision},
		{"foreign", root.RootRef, root.Revision},
	} {
		if err := bind(selection[0], selection[1], selection[2]); err == nil || len(list().Consents) != 0 {
			test.Fatal("unchecked storage root acquired consent", selection, err)
		}
	}
	if err := bind(root.RootSlot, root.RootRef, root.Revision); err != nil {
		test.Fatal(err)
	}
	bound := list()
	if len(bound.Consents) != 1 || len(bound.Roots) != 1 || !bound.Roots[0].Consented {
		test.Fatal("selected storage root was not consented", bound)
	}
	var output bytes.Buffer
	if err := sessionSourcesTo(&output, []string{"unbind", root.Agent, "--expected", "stale"}); err == nil ||
		len(list().Consents) != 1 {
		test.Fatal("stale identity released storage consent", err)
	}
	if err := manager.SetEnabled(test.Context(), installed.Manifest.ID, false); err != nil {
		test.Fatal(err)
	}
	if disabled := list(); len(disabled.Roots) != 0 || len(disabled.Consents) != 1 {
		test.Fatal("disabled module lost its releasable consent", disabled)
	}
	if err := sessionSourcesTo(&output, []string{"unbind", root.Agent, "--expected", bound.Consents[0].Identity}); err != nil {
		test.Fatal("disabled storage source could not release its exact consent", err)
	}
	if len(list().Consents) != 0 {
		test.Fatal("storage CLI retained consent after checked release")
	}
	if sessions.Reads(root.Agent) {
		test.Fatal("storage CLI granted ordinary history reads")
	}
}
