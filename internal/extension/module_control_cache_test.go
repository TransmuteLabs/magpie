package extension

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func installRuntimeConfigFixture(test testing.TB, manager *Manager, manifest sdk.Manifest) Installed {
	test.Helper()
	source := test.TempDir()
	body, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("unexecuted fixture"), 0o700); err != nil {
		test.Fatal(err)
	}
	entry, err := manager.catalog.Install(test.Context(), source, catalogTestCheck)
	if err != nil {
		test.Fatal(err)
	}
	return entry
}

func runtimeConfigFixture(test testing.TB, fields int) (*Manager, Installed) {
	test.Helper()
	manifest := sdk.Manifest{
		Schema: 1, ID: "runtime-config", Version: "1.0.0", Entry: "worker", Runtime: "exec",
		Contract:   sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: sdk.ModuleSettingsAPIMinor}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Activation: []string{sdk.ActivationGateway},
		Actions:    []sdk.ActionDescriptor{{ID: "read", Effect: "read", InputSchema: json.RawMessage(`{}`)}},
		Settings: []sdk.FeatureSetting{
			{Name: "mode", Kind: sdk.FeatureSettingString, Title: "Mode", Default: "fast", Enum: []any{"fast", "safe"}},
			{Name: "label", Kind: sdk.FeatureSettingString, Title: "Label", Default: "first"},
		},
	}
	manager := NewManager(test.TempDir())
	entry := installRuntimeConfigFixture(test, manager, manifest)
	values := json.RawMessage(`{"mode":"fast"}`)
	if fields != 0 {
		body, err := json.Marshal(map[string]any{"mode": "fast", "private": makeRuntimeConfigFixtureValues(fields)})
		if err != nil {
			test.Fatal(err)
		}
		values = body
	}
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), entry.Manifest.ID, "0", values); err != nil {
		test.Fatal(err)
	}
	return manager, entry
}

func makeRuntimeConfigFixtureValues(fields int) []string {
	values := make([]string, fields)
	for index := range values {
		values[index] = strings.Repeat("x", 64)
	}
	return values
}

func BenchmarkRuntimeConfigValidation(bench *testing.B) {
	for _, fixture := range []struct {
		name   string
		fields int
	}{{"small", 0}, {"populated", 120}} {
		bench.Run(fixture.name+"/reconcile", func(bench *testing.B) {
			manager, entry := runtimeConfigFixture(bench, fixture.fields)
			bench.ReportAllocs()
			for bench.Loop() {
				current, err := manager.reconcile(bench.Context(), true)
				if err != nil || current[entry.Manifest.ID].config.Version != "1" {
					bench.Fatalf("reconciliation did not select the configured module: %v", err)
				}
			}
		})
		bench.Run(fixture.name+"/generation", func(bench *testing.B) {
			manager, entry := runtimeConfigFixture(bench, fixture.fields)
			bench.ReportAllocs()
			for bench.Loop() {
				if err := manager.checkActiveGeneration(bench.Context(), entry, "1"); err != nil {
					bench.Fatal(err)
				}
			}
		})
	}
}

func TestRuntimeConfigUsesCurrentBytesAndReturnsOwnedValues(t *testing.T) {
	manager, entry := runtimeConfigFixture(t, 0)
	id := entry.Manifest.ID
	reconcile := func(want string) sdk.ConfigSnapshot {
		t.Helper()
		current, err := manager.reconcile(t.Context(), true)
		if err != nil {
			t.Fatal(err)
		}
		config, found := current[id]
		if want == "" {
			if found {
				t.Fatal("reconciliation selected an invalid configuration")
			}
		} else if !found || config.config.Version != "1" || string(config.config.Values) != want {
			t.Fatalf("unexpected effective configuration: %q", config.config.Values)
		}
		return config.config
	}
	published := 0
	check := func(expected Installed, available bool) {
		t.Helper()
		before := published
		err := manager.withActiveGeneration(t.Context(), expected, "1", func() error {
			published++
			return nil
		})
		if (err == nil) != available || (published == before+1) != available {
			t.Fatalf("generation admission for %s: available=%t, publications=%d, error=%v", expected.Revision, available, published-before, err)
		}
	}
	initial := reconcile(`{"label":"first","mode":"fast"}`)
	initial.Values[0] = '['
	reconcile(`{"label":"first","mode":"fast"}`)
	check(entry, true)

	path := filepath.Join(manager.Namespace(), "private", id, "state", storeStateName(sdk.Scope{Plugin: id}, moduleConfigKey))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replace := func(content []byte) {
		t.Helper()
		if len(content) != len(original) {
			t.Fatal("replacement changed the file size")
		}
		next := path + ".replacement"
		catalogTestWrite(t, next, content, 0o600)
		if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, path); err != nil {
			t.Fatal(err)
		}
	}

	safe := bytes.Replace(original, []byte(`"fast"`), []byte(`"safe"`), 1)
	replace(safe)
	reconcile(`{"label":"first","mode":"safe"}`)
	check(entry, true)
	replace(bytes.Replace(original, []byte(`"fast"`), []byte(`"junk"`), 1))
	reconcile("")
	check(entry, false)
	replace(bytes.Repeat([]byte{'?'}, len(original)))
	check(entry, false)
	reconcile("")
	replace(safe)
	reconcile(`{"label":"first","mode":"safe"}`)

	updated := entry.Manifest.Clone()
	updated.Version = "2.0.0"
	updated.Settings[1].Default = "second"
	replacement := installRuntimeConfigFixture(t, manager, updated)
	check(entry, false)
	reconcile(`{"label":"second","mode":"safe"}`)
	check(replacement, true)

	config, err := manager.CompareAndSwapModuleConfig(t.Context(), id, "1", json.RawMessage(`{"mode":"safe"}`))
	if err != nil || config.Version != "2" {
		t.Fatalf("saving the same values must advance the CAS version: %v, %v", config.Version, err)
	}
	check(replacement, false)
	if err := manager.checkActiveGeneration(t.Context(), replacement, "2"); err != nil {
		t.Fatal(err)
	}
	current, err := manager.reconcile(t.Context(), true)
	if err != nil || current[id].config.Version != "2" || string(current[id].config.Values) != `{"label":"second","mode":"safe"}` {
		t.Fatalf("reconciliation must use the current CAS version: %+v, %v", current[id].config, err)
	}

	updated.Version = "3.0.0"
	updated.Settings[0].Enum = []any{"fast"}
	replacement = installRuntimeConfigFixture(t, manager, updated)
	reconcile("")
	if err := manager.checkActiveGeneration(t.Context(), replacement, "2"); err == nil {
		t.Fatal("saved values should be validated against the new schema")
	}
}
