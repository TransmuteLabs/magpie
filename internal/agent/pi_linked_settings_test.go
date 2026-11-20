package agent

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestPiLinkedModelPickKeepsConcurrentScopeEdit(t *testing.T) {
	client := pi(syncHome(t))
	const initial = `{"defaultProvider":"mine","defaultModel":"first","enabledModels":["mine/first"]}`
	const user = `{"defaultProvider":"mine","defaultModel":"first","enabledModels":["mine/first","mine/another"]}`
	writeFile(t, client.Path, initial)
	path, err := edit.Target(client.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(target string, body []byte) []byte {
		if target == path && !edited {
			edited = true
			writeFile(t, target, user)
		}
		return body
	}, func() error { return client.Apply("model", "mine/new") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(client.Path) != user {
		t.Fatalf("edited = %t, err = %v, settings = %s", edited, err, readFile(client.Path))
	}
}

func TestPiLinkedModelResetKeepsSettingsWhenCatalogIsUnreadable(t *testing.T) {
	client := pi(syncHome(t))
	const settings = `{"defaultProvider":"magpie","defaultModel":"relay/old","enabledModels":["magpie/relay/old","mine/first"]}`
	const broken = `{"providers":`
	catalog := filepath.Join(client.Dir, "models.json")
	writeFile(t, client.Path, settings)
	writeFile(t, catalog, broken)
	err := client.Field("model").Set("")
	if err == nil || readFile(client.Path) != settings || readFile(catalog) != broken {
		t.Fatalf("err = %v, settings = %s, catalog = %s", err, readFile(client.Path), readFile(catalog))
	}
}

func TestPiLinkedModelResetRollsBackWhenCatalogChanges(t *testing.T) {
	client := pi(syncHome(t))
	const settings = `{"defaultProvider":"magpie","defaultModel":"relay/old","enabledModels":["magpie/relay/old","mine/first"]}`
	const catalog = `{"providers":{"magpie":{"api":"openai-completions"}}}`
	const user = `{"providers":{"magpie":{"api":"openai-completions"},"mine":{"api":"anthropic-messages"}}}`
	path := filepath.Join(client.Dir, "models.json")
	writeFile(t, client.Path, settings)
	writeFile(t, path, catalog)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !edited {
			edited = true
			writeFile(t, path, user)
		}
		return body
	}, func() error { return client.Field("model").Set("") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(client.Path) != settings || readFile(path) != user {
		t.Fatalf("edited = %t, err = %v, settings = %s, catalog = %s", edited, err, readFile(client.Path), readFile(path))
	}
}

func TestPiLinkedSyncKeepsConcurrentScopeEdit(t *testing.T) {
	home := syncHome(t)
	wevnalSave(t, "kimi-k3", "glm-5.2")
	client := pi(home)
	const first = `{"enabledModels":["magpie/wevnal/glm-5.2","magpie/wevnal/kimi-k3"]}`
	const user = `{"enabledModels":["magpie/wevnal/glm-5.2","magpie/wevnal/kimi-k3","mine/another"]}`
	writeFile(t, client.Path, first)
	if err := client.Apply("model", "magpie/wevnal/glm-5.2"); err != nil {
		t.Fatal(err)
	}
	wevnalSave(t, "glm-5.2")
	path, err := edit.Target(client.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(target string, body []byte) []byte {
		if target == path && !edited {
			edited = true
			writeFile(t, target, user)
		}
		return body
	}, client.Sync)
	buried := scopeGoneLoad(client.ID + ".scope.gone")
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(client.Path) != user || len(buried.Entries) != 0 {
		t.Fatalf("edited = %t, err = %v, settings = %s, pruned = %+v", edited, err, readFile(client.Path), buried)
	}
}

func TestPiLinkedRenameKeepsConcurrentScopeEdit(t *testing.T) {
	client := pi(syncHome(t))
	const initial = `{"enabledModels":["magpie/wevnal/kimi-k3","mine/first"]}`
	const user = `{"enabledModels":["magpie/wevnal/kimi-k3","mine/another"]}`
	writeFile(t, client.Path, initial)
	key := client.ID + ".scope.gone"
	scopeGoneSave(key, scopeGone{Entries: []string{"magpie/wevnal/glm-5.2"}})
	path, err := edit.Target(client.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(target string, body []byte) []byte {
		if target == path && !edited {
			edited = true
			writeFile(t, target, user)
		}
		return body
	}, func() error {
		_, err := client.RenameRefs("wevnal", "new")
		return err
	})
	buried := scopeGoneLoad(key)
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(client.Path) != user ||
		len(buried.Entries) != 1 || buried.Entries[0] != "magpie/wevnal/glm-5.2" {
		t.Fatalf("edited = %t, err = %v, settings = %s, pruned = %+v", edited, err, readFile(client.Path), buried)
	}
}
