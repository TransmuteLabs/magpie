package sessions

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCodexLineageRequiresRelatedCapability(test *testing.T) {
	source, host, _, _ := lineageFixture(test)
	host.entries[0].Manifest.Contract.Capabilities = slices.DeleteFunc(
		host.entries[0].Manifest.Contract.Capabilities,
		func(capability sdk.Capability) bool { return capability == "session-related-inputs" })
	_, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
		"external", sdk.Scope{}, nil, inputPolicy(test))
	if err == nil || host.method == sdk.MethodSessionConvert {
		test.Fatal("sparse history acquired or converted without the related-input capability", err)
	}
}

func TestCodexLineageCarrierPreservesSelectedOriginal(test *testing.T) {
	source, host, _, prefix := lineageFixture(test)
	original, err := os.ReadFile(source.Path)
	if err != nil {
		test.Fatal(err)
	}
	_, _, err = convertExtensionSource(test.Context(), host, "external", "conversation", source,
		"external", sdk.Scope{}, nil, inputPolicy(test))
	if err != nil {
		test.Fatal(err)
	}
	if len(host.request.Files) != 1 || !bytes.Equal(host.request.Files[0].Body, original) {
		test.Fatal("core materialized vendor history instead of preserving the selected original")
	}
	body, err := json.Marshal(host.request)
	if err != nil {
		test.Fatal(err)
	}
	var wire struct {
		Related struct {
			Version   int    `json:"version"`
			Kind      string `json:"kind"`
			Ancestors []struct {
				Body []byte `json:"body"`
			} `json:"ancestors"`
		} `json:"related"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		test.Fatal(err)
	}
	if wire.Related.Version != 1 || wire.Related.Kind != "codex-rollout-lineage" ||
		len(wire.Related.Ancestors) != 1 || !bytes.Equal(wire.Related.Ancestors[0].Body, prefix) {
		test.Fatal("carrier did not preserve the exact decoded ancestor prefix")
	}
	if !bytes.Contains(host.planning.Files[0].Body, []byte(`"history_base":{`)) {
		test.Fatal("session.inputs.v1 no longer received the original selected document")
	}
}

func TestCodexLineageCarrierRevisionBindsExcludedOriginalSuffix(test *testing.T) {
	source, host, parent, _ := lineageFixture(test)
	policy := inputPolicy(test)
	first, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
		"external", sdk.Scope{}, nil, policy)
	if err != nil {
		test.Fatal(err)
	}
	body, err := os.ReadFile(parent)
	if err != nil {
		test.Fatal(err)
	}
	body = append(body, lineageMessage(test, 4, "user", "Another excluded ancestor suffix")...)
	if err := os.WriteFile(parent, body, 0o600); err != nil {
		test.Fatal(err)
	}
	second, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
		"external", sdk.Scope{}, nil, policy)
	if err != nil {
		test.Fatal(err)
	}
	if first.Preview().Revision == second.Preview().Revision {
		test.Fatal("plan revision omitted the full original capture identity outside the inherited prefix")
	}
	if _, err := first.Execute(test.Context(), first.Preview().Revision); err == nil {
		test.Fatal("old plan accepted a changed excluded original suffix")
	}
}

func TestCodexLineageActionTargetAcceptsOnlyCapturedCreatedParents(test *testing.T) {
	lineageFixture(test)
	store, err := resolveSessionTargetStore(sdk.SessionDescriptor{To: "omp", OutputDir: ".omp/agent/sessions"})
	if err != nil {
		test.Fatal(err)
	}
	if err := store.check(test.Context()); err != nil {
		test.Fatal(err)
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		test.Fatal(err)
	}
	created := make(map[string]fs.FileInfo)
	for _, directory := range store.dirs {
		if directory.info == nil {
			info, err := os.Lstat(filepath.Join(store.root, directory.path))
			if err != nil {
				test.Fatal(err)
			}
			created[filepath.ToSlash(directory.path)] = info
		}
	}
	if len(created) == 0 {
		test.Fatal("fixture did not create previously missing target parents")
	}
	if err := checkSessionTargetDuringAction(test.Context(), store, nil); err == nil {
		test.Fatal("unowned target appearance acquired publication authority")
	}
	if err := checkSessionTargetDuringAction(test.Context(), store, created); err != nil {
		test.Fatal("action-owned target parents invalidated original source revalidation", err)
	}
	if err := store.check(test.Context()); err == nil {
		test.Fatal("per-action target validation changed the original target snapshot")
	}
	if err := os.Rename(store.directory, store.directory+".retained"); err != nil {
		test.Fatal(err)
	}
	if err := os.Mkdir(store.directory, 0o700); err != nil {
		test.Fatal(err)
	}
	if err := checkSessionTargetDuringAction(test.Context(), store, created); err == nil {
		test.Fatal("replaced action-created target parent retained publication authority")
	}
}
