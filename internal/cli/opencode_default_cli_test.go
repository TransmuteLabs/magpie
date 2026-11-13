package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

func openCodeVersionFixture(t *testing.T, version string) {
	t.Helper()
	bin := t.TempDir()
	command := filepath.Join(bin, "opencode")
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
		if err := os.WriteFile(command+".cmd", []byte("@echo "+version+"\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		testenv.Program(t, command, "#!/bin/sh\necho "+version+"\n")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// `magpie opencode a/m` wires OpenCode to magpie; `magpie opencode default`
// puts back the model OpenCode was on before (#1900), as the Agents page's
// Disconnect does. DevEco Code, OpenCode's fork with its own folder, the
// same.
func TestAgentOpenCodeDefaultPutsBackTheModel(t *testing.T) {
	for _, c := range []struct{ id, file string }{
		{"opencode", "opencode/opencode.json"},
		{"deveco", "deveco/deveco.json"},
	} {
		t.Run(c.id, func(t *testing.T) {
			openCodeVersionFixture(t, "1.2.7")
			groupsHome(t)
			path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), c.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			own := `{
  "$schema": "https://opencode.ai/config.json",
  "model": "native/glm-5",
  "small_model": "native/glm-5-mini",
  "theme": "dark"
}`
			if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
				t.Fatal(err)
			}
			read := func() string {
				b, _ := os.ReadFile(path)
				return string(b)
			}
			if err := run([]string{c.id, "a/m"}); err != nil {
				t.Fatal(err)
			}
			s := read()
			if !strings.Contains(s, `"model": "magpie/a/m"`) || !strings.Contains(s, `"provider"`) {
				t.Fatalf("not wired:\n%s", s)
			}
			if err := run([]string{c.id, "default"}); err != nil {
				t.Fatal(err)
			}
			s = read()
			if !strings.Contains(s, `"model": "native/glm-5"`) || !strings.Contains(s, `"small_model": "native/glm-5-mini"`) || strings.Contains(s, `"magpie"`) || strings.Contains(s, "baseURL") || !strings.Contains(s, `"theme": "dark"`) {
				t.Fatalf("default didn't put back what was there:\n%s", s)
			}
		})
	}
}

func TestAgentOpenCode2DefaultPutsBackTheModel(t *testing.T) {
	openCodeVersionFixture(t, "2.2.6")
	groupsHome(t)
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	own := `{
  "$schema": "https://opencode.ai/config.json",
  "model": "native/glm-5",
  "agents": {"title": {"model": "native/glm-5-mini", "temperature": 0.2}},
  "providers": {"mine": {"package": "aisdk:@ai-sdk/openai-compatible", "settings": {"apiKey": "theirs"}}},
  "theme": "dark"
}`
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"opencode", "a/m"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wired struct {
		Model     string          `json:"model"`
		Provider  json.RawMessage `json:"provider"`
		Providers map[string]struct {
			Package string `json:"package"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &wired); err != nil {
		t.Fatal(err)
	}
	if wired.Model != "magpie/a/m" || len(wired.Provider) != 0 || wired.Providers["magpie"].Package != "aisdk:@ai-sdk/openai-compatible" {
		t.Fatalf("not wired in OpenCode 2's schema:\n%s", body)
	}
	if err := run([]string{"opencode", "default"}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	if err := json.Unmarshal([]byte(own), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("OpenCode 2 default did not restore the model and preserve the user's settings:\n%s", body)
	}
}
