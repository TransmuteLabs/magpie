package agent

import (
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestProtocolPickKeepsPriorChoiceOnCatalogConflict(t *testing.T) {
	for _, id := range []string{"pi", "omp"} {
		t.Run(id, func(t *testing.T) {
			home := syncHome(t)
			at := here(home)
			agent := pi(home)
			catalog := filepath.Join(agent.Dir, "models.json")
			protocol := piProtocolKey(at)
			writeAPI := func() error {
				return edit.SetJSON(catalog, edit.KV{Path: "providers.magpie.api", Value: "openai-responses"})
			}
			userEdit := func() string { return strings.Replace(readFile(catalog), "{", "{\n\t\"userComment\": \"kept\",", 1) }
			catalogAPI := func() string {
				value, _ := edit.GetJSON(catalog, "providers.magpie.api")
				return value
			}
			if id == "omp" {
				agent = omp(home)
				catalog = filepath.Join(agent.Dir, "models.yml")
				protocol = ompProtocolKey(at, agent.Dir)
				writeAPI = func() error {
					return edit.SetYAML(catalog, edit.KV{Path: "providers.magpie.api", Value: "openai-responses"})
				}
				userEdit = func() string { return readFile(catalog) + "userComment: kept\n" }
				catalogAPI = func() string {
					value, _ := edit.GetYAML(catalog, "providers.magpie.api")
					return value
				}
			}
			if err := agent.Apply("model", "magpie/relay/glm-4.6"); err != nil {
				t.Fatal(err)
			}
			if err := writeAPI(); err != nil {
				t.Fatal(err)
			}
			keys := []string{protocol, apisWantedKey(catalog), outputsWantedKey(catalog)}
			before := peek(keys...)
			user := userEdit()
			target, err := edit.Target(catalog)
			if err != nil {
				t.Fatal(err)
			}
			var during map[string]string
			err = edit.Filtered(func(path string, data []byte) []byte {
				if path == target && during == nil {
					during = peek(keys...)
					writeFile(t, catalog, user)
				}
				return data
			}, func() error { return agent.Apply("protocol", "anthropic-messages") })
			if during == nil || !maps.Equal(during, before) || !errors.Is(err, extension.ErrActionChanged) ||
				readFile(catalog) != user || !maps.Equal(peek(keys...), before) {
				t.Fatalf("error = %v, user edit retained = %t, metadata at write = %v, final = %v, original = %v",
					err, readFile(catalog) == user, during, peek(keys...), before)
			}
			if err := agent.Sync(); err != nil {
				t.Fatal(err)
			}
			if got := catalogAPI(); got != "openai-responses" {
				t.Fatalf("sync applied rejected pick: %q", got)
			}
			if err := agent.Apply("protocol", "anthropic-messages"); err != nil {
				t.Fatal(err)
			}
			if got := catalogAPI(); got != "anthropic-messages" || peek(protocol)[protocol] != got ||
				!strings.Contains(readFile(catalog), "userComment") {
				t.Fatalf("retry API = %q, metadata = %v, catalog = %q", got, peek(protocol), readFile(catalog))
			}
		})
	}
}
