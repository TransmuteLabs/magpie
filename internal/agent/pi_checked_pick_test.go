package agent

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestPiMagpiePickRollsBackCatalogWhenScopeChanges(t *testing.T) {
	client := pi(syncHome(t))
	catalog := filepath.Join(client.Dir, "models.json")
	const initial = `{"defaultProvider":"magpie","defaultModel":"relay/old","enabledModels":["magpie/relay/old"]}`
	const user = `{"defaultProvider":"magpie","defaultModel":"relay/old","enabledModels":["magpie/relay/old","mine/another"]}`
	const originalCatalog = `{"providers":{"magpie":{"models":[{"id":"relay/old"}]}}}`
	writeFile(t, client.Path, initial)
	writeFile(t, catalog, originalCatalog)
	target, err := edit.Target(client.Path)
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
	}, func() error { return client.Apply("model", "magpie/relay/glm-4.6") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Pi accepted the changed scope: edited=%t err=%v", edited, err)
	}
	if readFile(client.Path) != user || readFile(catalog) != originalCatalog {
		t.Errorf("Pi left a partial pick: settings=%s catalog=%s", readFile(client.Path), readFile(catalog))
	}
	if stashShared()[apisWantedKey(catalog)] != "" || stashShared()[outputsWantedKey(catalog)] != "" {
		t.Error("Pi saved catalog history for a rejected pick")
	}
	if err := client.Apply("model", "magpie/relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	if model, _ := edit.GetJSON(client.Path, "defaultModel"); model != "relay/glm-4.6" {
		t.Errorf("the retry picked %q", model)
	}
	if scope := piScope(client.Path); len(scope) != 3 || scope[1] != "mine/another" || scope[2] != "magpie/relay/glm-4.6" {
		t.Errorf("the retry did not keep the user's scope: %v", scope)
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
