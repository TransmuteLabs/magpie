package agent

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestOmpSyncScopeRejectsConcurrentEditWithoutPublishingCatalog(t *testing.T) {
	home, config, catalog := ompScopeHome(t)
	wevnalSave(t, wevnalAll...)
	a := omp(home)
	if err := a.Apply("model", "magpie/wevnal/kimi-k3"); err != nil {
		t.Fatal(err)
	}
	before := readFile(catalog)
	const initial = "# mine\nenabledModels:\n  - magpie/wevnal/space-bunny-free:high\n  - magpie/wevnal/kimi-k3\n"
	const user = initial + "theme: light\n"
	writeFile(t, config, initial)
	wevnalSave(t, "kimi-k3")
	target, err := edit.Target(config)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == target && !edited {
			writeFile(t, config, user)
			edited = true
		}
		return data
	}, a.Sync)
	gone := here(home).key("omp.scope.gone")
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(config) != user ||
		readFile(catalog) != before || peek(gone)[gone] != "" {
		t.Fatalf("edited = %t, error = %v, config = %q, catalog changed = %t, stash = %q",
			edited, err, readFile(config), readFile(catalog) != before, peek(gone)[gone])
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(config); strings.Contains(got, "space-bunny-free") || !strings.Contains(got, "theme: light") ||
		!strings.Contains(peek(gone)[gone], "space-bunny-free:high") || readFile(catalog) == before {
		t.Fatalf("retry config = %q, stash = %q, catalog changed = %t", got, peek(gone)[gone], readFile(catalog) != before)
	}
}

func TestOmpRenameRejectsConcurrentEditWithoutRenamingStash(t *testing.T) {
	home := syncHome(t)
	a := omp(home)
	const initial = "modelRoles:\n  task: [magpie/relay/glm-4.6:high, openai/gpt-6] # mine\nenabledModels: [magpie/relay/glm-4.6, magpie/new/glm-4.6]\n"
	const user = initial + "theme: light\n"
	writeFile(t, a.Path, initial)
	role := "omp:" + a.Path + ":modelRoles.slow"
	gone := here(home).key("omp.scope.gone")
	const fallback = "[magpie/relay/glm-4.6, openai/gpt-6]"
	const removed = `{"entries":["magpie/relay/deselected:low"]}`
	stash(map[string]string{role: fallback, gone: removed})
	target, err := edit.Target(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited, moved := false, false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == target && !edited {
			writeFile(t, a.Path, user)
			edited = true
		}
		return data
	}, func() error {
		moved, err = a.RenameRefs("relay", "new")
		return err
	})
	saved := peek(role, gone)
	if !edited || moved || !errors.Is(err, extension.ErrActionChanged) || readFile(a.Path) != user ||
		saved[role] != fallback || saved[gone] != removed {
		t.Fatalf("edited = %t, moved = %t, error = %v, config = %q, stash = %v", edited, moved, err, readFile(a.Path), saved)
	}
	if moved, err = a.RenameRefs("relay", "new"); !moved || err != nil {
		t.Fatalf("retry moved = %t, error = %v", moved, err)
	}
	saved = peek(role, gone)
	if got := readFile(a.Path); !strings.Contains(got, "task: [") || !strings.Contains(got, "] # mine") ||
		!strings.Contains(got, "theme: light") ||
		!slices.Equal(edit.GetYAMLList(a.Path, "modelRoles.task"), []string{"magpie/new/glm-4.6:high", "openai/gpt-6"}) ||
		!slices.Equal(edit.GetYAMLList(a.Path, "enabledModels"), []string{"magpie/new/glm-4.6"}) ||
		saved[role] != "[magpie/new/glm-4.6, openai/gpt-6]" ||
		!strings.Contains(saved[gone], "magpie/new/deselected:low") {
		t.Fatalf("retry config = %q, stash = %v", got, saved)
	}
	if got := readFile(filepath.Join(a.Dir, "models.yml")); got != "" {
		t.Fatalf("rename wrote the catalog: %q", got)
	}
}
