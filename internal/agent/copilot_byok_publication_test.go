package agent

import (
	"errors"
	"os"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestCopilotBYOKModelPublication(t *testing.T) {
	for _, operation := range []string{"pick", "clear"} {
		t.Run(operation, func(t *testing.T) {
			cp, catalog := copilotSandbox(t)
			const userCatalog = `{"providers":[{"name":"mine","type":"openai"}],"models":[{"id":"own","provider":"mine"}],"theme":"x"}`
			writeFile(t, catalog, userCatalog)
			writeFile(t, cp.Path, `{"model":"mine/own","effortLevel":"high"}`)

			model, first, second := "magpie/deepseek/pro", catalog, cp.Path
			userFile := `{"model":"mine/edited","effortLevel":"high"}`
			if operation == "clear" {
				if err := cp.Apply("model", model); err != nil {
					t.Fatal(err)
				}
				model, first, second = "", cp.Path, catalog
				userFile = `{"providers":[{"name":"mine","type":"openai"}],"models":[{"id":"other","provider":"mine"}],"theme":"y"}`
			}
			before := readFile(first)
			target, err := edit.Target(second)
			if err != nil {
				t.Fatal(err)
			}
			reached := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !reached {
					reached = true
					if readFile(first) == before {
						t.Error("first publication was not visible before the second")
					}
					if err := os.WriteFile(path, []byte(userFile), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return body
			}, func() error { return cp.Apply("model", model) })
			if !reached || !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("expected a stale second publication, reached=%t, error=%v", reached, err)
			}
			if got := readFile(first); got != before {
				t.Errorf("first publication was left in place:\n%s", got)
			}
			if got := readFile(second); got != userFile {
				t.Errorf("second publication replaced the user's change:\n%s", got)
			}
		})
	}
}

func TestCopilotBYOKCatalogSyncPreservesConcurrentEdits(t *testing.T) {
	cp, catalog := copilotSandbox(t)
	const before = `{"providers":[{"name":"magpie","type":"openai","baseUrl":"http://127.0.0.1:1/v1","wireApi":"responses"}],"models":[]}`
	const user = `{"providers":[{"name":"magpie","type":"openai","baseUrl":"http://127.0.0.1:1/v1","wireApi":"responses"}],"models":[],"theme":"dark"}`
	const config = `{"model":"magpie/deepseek/pro","effortLevel":"high"}`
	writeFile(t, catalog, before)
	writeFile(t, cp.Path, config)
	target, err := edit.Target(catalog)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !edited {
			edited = true
			if err := os.WriteFile(path, []byte(user), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return body
	}, cp.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(catalog) != user {
		t.Fatalf("catalog sync lost the concurrent edit, edited=%t, error=%v", edited, err)
	}
	if err := cp.Sync(); err != nil {
		t.Fatal(err)
	}
	providers, models, top := copilotParsed(t, catalog)
	if len(providers) != 1 || providers[0]["wireApi"] != "responses" ||
		len(models) == 0 || top["theme"] != "dark" || readFile(cp.Path) != config || cp.Check() != "" {
		t.Fatal("catalog retry did not repair the gateway and preserve the user's settings")
	}
}
