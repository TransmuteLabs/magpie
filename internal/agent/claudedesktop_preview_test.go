package agent

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeDesktopDisconnectPreview(t *testing.T) {
	for _, test := range []struct {
		name     string
		imported bool
		linked   bool
		empty    bool
	}{
		{name: "legacy"},
		{name: "imported_fields", imported: true},
		{name: "linked_profile", linked: true},
		{name: "empty_linked_profile", linked: true, empty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, paths := desktopSandbox(t)
			desktopWrite(t, paths.config, `{"deploymentMode":"3p","userEdit":{"token":"dummy-user-token"}}`)
			desktopWrite(t, paths.config3p, `{"deploymentMode":"3p","theme":"dark"}`)
			desktopWrite(t, paths.meta, `{"appliedId":"`+desktopProfileID+`","entries":[{"id":"`+desktopProfileID+`","name":"magpie"}],"userEdit":true}`)
			profileBody := `{"inferenceGatewayApiKey":"dummy-managed-key","toolSearchEnabled":true}`
			if test.empty {
				profileBody = "{}"
			}
			desktopWrite(t, paths.prof, profileBody)
			desktopWrite(t, paths.app3p, `{"locale":"en-US"}`)
			desktopWrite(t, filepath.Join(paths.dir3p, "claude-code-sessions", "kept.json"), `{"sentinel":"unchanged history"}`)
			saved := map[string]string{
				desktopStashed: "1",
				desktopMade:    strings.Join([]string{paths.meta, paths.prof, paths.app3p}, "\n"),
				desktopLocale:  "en-US",
			}
			imported := filepath.Join(paths.library, "imported.json")
			alias := filepath.Join(home, "profile-alias.json")
			if test.imported {
				desktopWrite(t, imported, `{"inferenceGatewayApiKey":"dummy-current-key","inferenceGatewayAuthScheme":"bearer","userEdit":true}`)
				desktopWrite(t, paths.meta, `{"appliedId":"`+desktopProfileID+`","entries":[{"id":"`+desktopProfileID+`"},{"id":"imported","auth":{"value":"dummy-metadata-secret"}}]}`)
				desktopWrite(t, paths.config3p, `{"deploymentMode":"3p","mcpServers":{"carried":{"env":{"CUSTOM":"dummy-mcp-secret"}},"kept":{"args":["dummy-unrelated-arg"]}}}`)
				saved[desktopApplied] = "imported"
				saved[desktopMCP] = `{"carried":{"env":{"CUSTOM":"dummy-mcp-secret"}}}`
				claims, err := json.Marshal(desktopImportedClaims{"imported": {
					desktopAPIKey:     {Before: json.RawMessage(`"dummy-prior-key"`), Hash: desktopValueHash("dummy-current-key")},
					desktopAuthScheme: {Hash: desktopValueHash("bearer")},
				}})
				if err != nil {
					t.Fatal(err)
				}
				saved[desktopImported] = string(claims)
			}
			if test.linked {
				if err := os.Link(paths.prof, alias); err != nil {
					t.Fatal(err)
				}
			}
			stashBody, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			desktopWrite(t, stashPath(), string(stashBody))
			before := desktopTree(t, paths)
			row := claudeDesktop(home)

			changes := previewOf(t, row)
			if got, err := os.ReadFile(stashPath()); err != nil || string(got) != string(stashBody) || !maps.Equal(before, desktopTree(t, paths)) {
				t.Fatal("preview changed the original settings, stash, or history")
			}
			if len(changes) == 0 {
				t.Fatal("preview returned no changes for the wired Claude Desktop files")
			}
			files := make(map[string]FileChange, len(changes))
			for _, change := range changes {
				files[change.Path] = change
			}
			find := func(path string) FileChange {
				t.Helper()
				rel, err := filepath.Rel(home, path)
				if err != nil {
					t.Fatal(err)
				}
				shown := "~/" + filepath.ToSlash(rel)
				change, ok := files[shown]
				if !ok {
					t.Errorf("preview is missing %s", shown)
				}
				return change
			}
			expect := func(path, op, text string) {
				t.Helper()
				if !previewSays([]FileChange{find(path)}, op, text) {
					t.Errorf("%s lacks %q for %q", filepath.Base(path), op, text)
				}
			}
			expect(paths.config, "-", `"deploymentMode": "3p"`)
			expect(paths.config3p, "-", `"deploymentMode": "3p"`)
			if test.imported {
				expect(paths.meta, "~", `"appliedId": "imported"`)
				expect(paths.meta, "~", `"entries"`)
				expect(paths.config3p, "~", `"carried"`)
				expect(imported, "~", `"inferenceGatewayApiKey"`)
				expect(imported, "-", `"inferenceGatewayAuthScheme"`)
			} else {
				expect(paths.meta, "-", `"appliedId"`)
				expect(paths.meta, "-", `"entries"`)
			}
			profile := find(paths.prof)
			if profile.Removed == test.linked || profile.Removed && len(profile.Lines) != 0 {
				t.Errorf("unexpected removal for the managed profile: %+v", profile)
			}
			if test.linked {
				if test.empty {
					if profile.Created || len(profile.Lines) != 1 || profile.Lines[0] != (LineDiff{Op: "-", Text: "{}"}) {
						t.Error("empty linked profile lacks an emptied-file line")
					}
				} else {
					expect(paths.prof, "-", `"inferenceGatewayApiKey"`)
				}
				if body, err := os.ReadFile(alias); err != nil || string(body) != before[paths.prof] {
					t.Fatal("preview changed the managed profile's other hard link")
				}
			}
			if app := find(paths.app3p); !app.Removed || len(app.Lines) != 0 {
				t.Errorf("preview did not simply mark the carried app settings for removal: %+v", app)
			}
			count := 5
			if test.imported {
				count++
			}
			if len(changes) != count || len(files) != count {
				t.Errorf("preview reports %d changes (%d paths), want %d", len(changes), len(files), count)
			}
			body, err := json.Marshal(changes)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "dummy-") || strings.Contains(string(body), `"userEdit"`) {
				t.Errorf("preview disclosed a secret or unchanged setting: %s", body)
			}
			if test.empty {
				prior, err := os.Stat(alias)
				if err != nil {
					t.Fatal(err)
				}
				if err := row.Disconnect(); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{paths.prof, alias} {
					current, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(prior, current) || current.Size() != 0 {
						t.Errorf("%s did not keep the original link emptied", filepath.Base(path))
					}
				}
			}
		})
	}
}
