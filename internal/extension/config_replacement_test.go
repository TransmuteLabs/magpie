package extension

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestRuntimeConfigReplacementFencesWorkerAtSameVersion(t *testing.T) {
	manager, entry := fixtureManager(t)
	ctx, id := t.Context(), entry.Manifest.ID
	saved, err := manager.CompareAndSwapModuleConfig(ctx, id, "0", json.RawMessage(`{"mode":"fast","private":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request := sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"pid"}`)}
	call := func() int {
		t.Helper()
		result, err := manager.Call(ctx, id, sdk.MethodToolCall, sdk.Scope{}, request, nil)
		if err != nil {
			t.Fatal(err)
		}
		var response struct{ PID int }
		if err := json.Unmarshal(result, &response); err != nil || response.PID <= 0 {
			t.Fatalf("invalid worker identity: %s, %v", result, err)
		}
		return response.PID
	}
	previous := call()
	old, err := manager.Acquire(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	replaceRuntimeConfigAtSameVersion(t, manager, id, `"mode":"fast","private":true`, `"private":true,"mode":"fast"`)
	if err := old.CheckCurrent(ctx); err != nil {
		t.Fatalf("reordered values revoked the lease: %v", err)
	}
	if current := call(); current != previous {
		t.Fatalf("reordered values restarted the worker: %d -> %d", previous, current)
	}

	replaceRuntimeConfigAtSameVersion(t, manager, id, `"mode":"fast"`, `"mode":"safe"`)
	config, err := manager.runtimeModuleConfigContext(ctx, entry)
	if err != nil || config.Version != saved.Version || !bytes.Contains(config.Values, []byte(`"safe"`)) {
		t.Fatalf("new configuration not loaded at version %q: %+v, %v", saved.Version, config, err)
	}
	if err := old.CheckCurrent(ctx); err == nil {
		t.Error("old lease remains current after replacing effective values")
	}
	if current := call(); current == previous {
		t.Errorf("execution reused the worker configured with old values: %d", current)
	}
}

func replaceRuntimeConfigAtSameVersion(t *testing.T, manager *Manager, id, oldBytes, newBytes string) {
	t.Helper()
	path := filepath.Join(manager.Namespace(), "private", id, "state", storeStateName(sdk.Scope{Plugin: id}, moduleConfigKey))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Replace(original, []byte(oldBytes), []byte(newBytes), 1)
	if bytes.Equal(body, original) || len(body) != len(original) {
		t.Fatalf("invalid replacement fixture: %q", body)
	}
	next := path + ".replacement"
	catalogTestWrite(t, next, body, 0o600)
	if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}
