package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/desktopdir"
	"github.com/yetone/magpie/internal/desktopiso"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/settings"
)

func TestClaudeDesktopEffectiveProfile(t *testing.T) {
	for _, scenario := range []struct {
		name, target, key, value string
	}{
		{"hybrid profile", "meta", "hybridPointer", `{"bootstrapUrl":"https://config.example.org/desktop"}`},
		{"profile forced first party", "profile", "deploymentMode", `"1p"`},
		{"invalid profile mode", "profile", "deploymentMode", `true`},
		{"interactive credential", "profile", "inferenceCredentialKind", `"interactive"`},
		{"imported external IDP", "imported", "inferenceCredentialKind", `"external-idp"`},
		{"helper credential", "profile", "inferenceCredentialKind", `"helper-script"`},
		{"legacy SSO", "profile", "inferenceGatewayAuthScheme", `"sso"`},
		{"legacy IDP", "profile", "inferenceGatewayOidc", `{"clientId":"desktop","issuer":"https://idp.example.test"}`},
		{"inferred external IDP", "profile", "inferenceIdpOidc", `{"clientId":"desktop","issuer":"https://idp.example.test"}`},
		{"inferred helper", "profile", "inferenceCredentialHelper", `"/path/to/credential-helper"`},
		{"self-hosted sessions", "profile", "selfHostedUrl", `"https://sessions.example.test"`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			home, paths := desktopSandbox(t)
			row := claudeDesktop(home)
			set := row.Field("provider").Set
			if err := set("magpie"); err != nil {
				t.Fatal(err)
			}
			path := paths.prof
			if scenario.target == "meta" {
				path = paths.meta
			}
			if scenario.target == "imported" {
				profile, err := os.ReadFile(paths.prof)
				if err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(paths.library, "imported.json")
				desktopWrite(t, path, string(profile))
				if err := edit.SetJSON(paths.meta, edit.KV{Path: "appliedId", Value: "imported"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := edit.SetJSON(path, edit.KV{Path: scenario.key, Value: json.RawMessage(scenario.value)}); err != nil {
				t.Fatal(err)
			}
			before, saved := desktopTree(t, paths), stashLoad()
			if message := row.Check(); !strings.Contains(message, scenario.key) {
				t.Errorf("Check = %q, want a conflict naming %s", message, scenario.key)
			}
			if err := row.Sync(); err == nil {
				t.Error("Sync accepted a conflicting Desktop profile")
			}
			if err := set("magpie"); err == nil {
				t.Error("Connect accepted a conflicting Desktop profile")
			}
			if !maps.Equal(before, desktopTree(t, paths)) || !maps.Equal(saved, stashLoad()) {
				t.Error("the conflicting profile or the saved originals changed")
			}
			if err := set(""); err != nil {
				t.Fatalf("disconnect after profile conflict: %v", err)
			}
		})
	}
}

func TestClaudeDesktopRestartAfterAppliedProfileChange(t *testing.T) {
	home := restartSandbox(t)
	wasOS := desktopdir.OS
	desktopdir.OS = "darwin"
	t.Cleanup(func() { desktopdir.OS = wasOS })
	apps := filepath.Join(home, "Applications")
	bundle := macApp(t, apps, "Claude.app", claudeDesktopApp.id, "Claude")
	restartOnMac = true
	appFolders = func() []string { return []string{apps} }
	alive, up := true, time.Hour
	binary := filepath.Join(bundle, "Contents", "MacOS", "Claude")
	running = func(pattern string) []runningProc {
		if alive && regexp.MustCompile(pattern).MatchString(binary) {
			return []runningProc{{up: up, cmd: binary}}
		}
		return nil
	}
	paths := desktopSets(desktopdir.OS, home, os.Getenv)[0]
	row := claudeDesktop(home)
	if err := row.Field("provider").Set("magpie"); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{paths.config, paths.config3p, paths.meta, paths.prof} {
		if err := os.Chtimes(path, timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	if row.AppBehind() != nil {
		t.Fatal("offered a restart for the current profile")
	}

	if err := edit.SetJSON(paths.meta, edit.KV{Path: "hybridPointer", Value: map[string]string{
		"bootstrapUrl": "https://config.example.test",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(paths.meta, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Check(), "hybridPointer") || row.AppBehind() == nil {
		t.Fatal("did not detect an effective profile change with unchanged timestamps")
	}
	var events []string
	appQuit = func(_ context.Context, path string) error {
		events = append(events, "quit "+path)
		alive = false
		return nil
	}
	appOpen = func(_ context.Context, path string) error {
		events = append(events, "open "+path)
		alive, up = true, 0
		return nil
	}
	if err := row.RestartApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"quit " + bundle, "open " + bundle}) || row.AppBehind() != nil {
		t.Fatalf("restart did not clear drift for %s: %q", bundle, events)
	}
}

func TestClaudeDesktopConnectRestartDisconnectKeepsProfileAndHistory(t *testing.T) {
	home := restartSandbox(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3425")
	claude := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	wasOS := desktopdir.OS
	desktopdir.OS = "darwin"
	t.Cleanup(func() { desktopdir.OS = wasOS })
	t.Cleanup(sessions.Reset)

	apps := filepath.Join(home, "Applications")
	bundle := macApp(t, apps, "Claude.app", claudeDesktopApp.id, "Claude")
	restartOnMac = true
	appFolders = func() []string { return []string{apps} }
	alive, up := true, time.Hour
	binary := filepath.Join(bundle, "Contents", "MacOS", "Claude")
	running = func(pattern string) []runningProc {
		if alive && regexp.MustCompile(pattern).MatchString(binary) {
			return []runningProc{{up: up, cmd: binary}}
		}
		return nil
	}
	var events []string
	appQuit = func(_ context.Context, path string) error {
		events = append(events, "quit "+path)
		alive = false
		return nil
	}
	appOpen = func(_ context.Context, path string) error {
		events = append(events, "open "+path)
		alive, up = true, 0
		return nil
	}

	paths := desktopSets(desktopdir.OS, home, os.Getenv)[0]
	const ownID = "11111111-2222-4333-8444-555555555555"
	own := filepath.Join(paths.library, ownID+".json")
	const initialProfile = "{\n  \"inferenceProvider\": \"bedrock\",\n  \"toolSearchEnabled\": false\n}\n"
	const editedProfile = "{\n  \"inferenceProvider\": \"bedrock\",\n  \"toolSearchEnabled\": true\n}\n"
	desktopWrite(t, own, initialProfile)
	desktopWrite(t, paths.config, `{"deploymentMode":"1p","globalShortcut":"Alt+Space"}`)
	desktopWrite(t, paths.config3p, `{"preferences":{"theme":"dark"}}`)
	desktopWrite(t, paths.meta, `{"appliedId":"`+ownID+`","entries":[{"id":"`+ownID+`","name":"Existing"}]}`)

	const sessionID = "22222222-3333-4444-8555-666666666666"
	const title = "Old Desktop session"
	const prompt = "Text from before Connect"
	const answer = "Answer from before Connect"
	const metadataJSON = `{"sessionId":"local_old","cliSessionId":"` + sessionID + `","title":"` + title + `"}`
	const transcriptJSON = `{"type":"user","uuid":"user","timestamp":"2026-11-01T12:00:00.000Z","cwd":"/work/app","sessionId":"` + sessionID + `","entrypoint":"claude-desktop","message":{"role":"user","content":"` + prompt + `"}}` + "\n" +
		`{"type":"assistant","uuid":"reply","parentUuid":"user","timestamp":"2026-11-01T12:00:01.000Z","cwd":"/work/app","sessionId":"` + sessionID + `","entrypoint":"claude-desktop","message":{"id":"msg_old","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"` + answer + `"}],"usage":{"input_tokens":7,"output_tokens":4}}}` + "\n"
	metadata := filepath.Join(paths.dir, "claude-code-sessions", "account", "org", "local_old.json")
	transcript := filepath.Join(claude, "projects", "-work-app", sessionID+".jsonl")
	desktopWrite(t, metadata, metadataJSON)
	desktopWrite(t, transcript, transcriptJSON)
	checkBytes := func(stage, path, want string) {
		t.Helper()
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s: %s = %q, %v, want %q", stage, path, got, err, want)
		}
	}
	checkHistory := func(stage string) {
		t.Helper()
		checkBytes(stage, metadata, metadataJSON)
		checkBytes(stage, transcript, transcriptJSON)
		sessions.Reset()
		if got := sessions.DesktopTitles([]string{sessionID})[sessionID]; got != title {
			t.Fatalf("%s: Desktop title = %q, want %q", stage, got, title)
		}
		old, ok := sessions.Find("claude", sessionID)
		if !ok || old.App != "claude-desktop" || old.Path != transcript {
			t.Fatalf("%s: old Desktop session = %+v, found %v", stage, old, ok)
		}
		history, err := sessions.TranscriptOf(old)
		var spoken []string
		for _, part := range history.Parts {
			spoken = append(spoken, part.Text)
		}
		if err != nil || !slices.Equal(spoken, []string{prompt, answer}) {
			t.Fatalf("%s: old transcript = %q, %v", stage, spoken, err)
		}
	}

	checkHistory("before Connect")
	row := claudeDesktop(home)
	if !row.Detected() || row.Wired() || row.AppBehind() != nil {
		t.Fatal("expected an unwired Desktop without a restart offer")
	}
	how, err := row.ConnectHow()
	if err != nil || how != (Connection{How: "magpie", Field: "provider", Value: "magpie"}) ||
		!row.Wired() || row.Check() != "" || row.AppBehind() == nil {
		t.Fatalf("Connect = %+v, %v, wired %v, check %q, restart %v", how, err, row.Wired(), row.Check(), row.AppBehind())
	}
	profile := desktopJSON(t, paths.prof)
	if desktopJSON(t, paths.meta)["appliedId"] != desktopProfileID ||
		desktopJSON(t, paths.config3p)["deploymentMode"] != "3p" ||
		profile["inferenceProvider"] != "gateway" || profile["inferenceGatewayBaseUrl"] != "http://127.0.0.1:3425" ||
		profile["inferenceGatewayApiKey"] != gateway.TokenFor("claude-desktop") || profile["inferenceGatewayAuthScheme"] != "bearer" {
		t.Fatalf("Connect did not select the Desktop gateway profile: %v", profile)
	}
	checkBytes("after Connect", own, initialProfile)
	checkHistory("after Connect")

	desktopWrite(t, own, editedProfile)
	if err := edit.SetJSON(paths.config, edit.KV{Path: "globalShortcut", Value: "Ctrl+Space"}); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetJSON(paths.config3p, edit.KV{Path: "preferences", Value: map[string]string{"theme": "light"}}); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetJSON(paths.meta, edit.KV{Path: "userSetting", Value: "after Connect"}); err != nil {
		t.Fatal(err)
	}
	beforeRestart := desktopTree(t, paths)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := row.RestartApp(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"quit " + bundle, "open " + bundle}) || row.AppBehind() != nil ||
		!maps.Equal(desktopTree(t, paths), beforeRestart) {
		t.Fatalf("Restart did not reopen only the fixture bundle with the same files: %q", events)
	}
	checkHistory("after Restart")

	if err := row.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if row.Wired() || !row.Disconnected() {
		t.Fatal("Desktop is still wired after Disconnect")
	}
	if _, err := os.Stat(paths.prof); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Magpie profile after Disconnect: %v", err)
	}
	for path, want := range map[string]map[string]any{
		paths.config:   {"deploymentMode": "1p", "globalShortcut": "Ctrl+Space"},
		paths.config3p: {"preferences": map[string]any{"theme": "light"}},
		paths.meta: {"appliedId": ownID, "entries": []any{map[string]any{"id": ownID, "name": "Existing"}},
			"userSetting": "after Connect"},
	} {
		if got := desktopJSON(t, path); !reflect.DeepEqual(got, want) {
			t.Errorf("after Disconnect, %s = %v, want %v", path, got, want)
		}
	}
	checkBytes("after Disconnect", own, editedProfile)
	checkHistory("after Disconnect")
}

func TestClaudeDesktopSettingsRollback(t *testing.T) {
	for _, action := range []string{"connect", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			home, paths := desktopSandbox(t)
			desktopWrite(t, paths.config, `{"deploymentMode":"1p","custom":true}`)
			desktopWrite(t, paths.config3p, `{"preferences":{"theme":"dark"}}`)
			desktopWrite(t, paths.meta, `{"entries":[{"id":"old","name":"Old"}],"appliedId":"old"}`)
			row := claudeDesktop(home)
			set := row.Field("provider").Set
			var next, failPath, editPath, user string
			if action == "connect" {
				next, failPath, editPath = "magpie", paths.config, paths.meta
				user = `{"entries":[{"id":"mine","name":"Mine"}],"appliedId":"mine"}`
			} else {
				if err := set("magpie"); err != nil {
					t.Fatal(err)
				}
				failPath, editPath = paths.meta, paths.prof
				user = `{"inferenceProvider":"gateway","userEdit":true}`
			}
			before, saved := desktopTree(t, paths), stashLoad()
			target, err := edit.Target(failPath)
			if err != nil {
				t.Fatal(err)
			}
			injected := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !injected {
					injected = true
					desktopWrite(t, editPath, user)
				}
				return body
			}, func() error { return set(next) })
			if !injected || !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("settings write did not reject a superseded snapshot, injected=%v, err=%v", injected, err)
			}
			before[editPath] = user
			if !maps.Equal(before, desktopTree(t, paths)) || !maps.Equal(saved, stashLoad()) {
				t.Error("partial settings update survived rollback or replaced the concurrent edit")
			}
			if err := set(next); err != nil {
				t.Fatalf("retry after conflict: %v", err)
			}
		})
	}
}

func TestClaudeDesktopIsolatedKeepsWiringOnSaveFailure(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "enable", false: "disable"}[on], func(t *testing.T) {
			home, usual, isolated, enabled := isolatedSandbox(t)
			if err := claudeDesktop(home).Field("provider").Set("magpie"); err != nil {
				t.Fatal(err)
			}
			if !on {
				if _, err := SetDesktopIsolated(home, true); err != nil {
					t.Fatal(err)
				}
			}
			beforeUsual, beforeIsolated := desktopTree(t, usual), desktopTree(t, isolated)
			beforeLauncher, beforeStash := desktopiso.Here(home), stashLoad()
			beforeCodeFolder := desktopIsolatedClaudeOn()
			save := saveDesktopIsolated
			sentinel := errors.New("cannot save isolated mode")
			saveDesktopIsolated = func(bool, bool, bool) error { return sentinel }
			_, err := SetDesktopIsolated(home, on)
			saveDesktopIsolated = save
			if !errors.Is(err, sentinel) {
				t.Fatalf("isolation = %v, want save error", err)
			}
			if !maps.Equal(beforeUsual, desktopTree(t, usual)) ||
				!maps.Equal(beforeIsolated, desktopTree(t, isolated)) ||
				!maps.Equal(beforeStash, stashLoad()) || beforeLauncher != desktopiso.Here(home) ||
				*enabled == on || beforeCodeFolder != desktopIsolatedClaudeOn() {
				t.Fatal("failed to keep wiring, stash and launcher on settings failure")
			}
			if on {
				if _, err := os.Stat(desktopIsolatedClaudeDir(home)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed enable left a Code folder: %v", err)
				}
			}
		})
	}
}

func TestClaudeDesktopIsolationRejectsUnreadableProfile(t *testing.T) {
	for _, enable := range []bool{true, false} {
		t.Run(map[bool]string{true: "enable", false: "disable"}[enable], func(t *testing.T) {
			home, usual, isolated, enabled := isolatedSandbox(t)
			if err := claudeDesktop(home).Field("provider").Set("magpie"); err != nil {
				t.Fatal(err)
			}
			broken := usual.meta
			if !enable {
				if _, err := SetDesktopIsolated(home, true); err != nil {
					t.Fatal(err)
				}
				broken = isolated.meta
			}
			original, err := os.ReadFile(broken)
			if err != nil {
				t.Fatal(err)
			}
			desktopWrite(t, broken, `{"entries":`)
			beforeUsual, beforeIsolated := desktopTree(t, usual), desktopTree(t, isolated)
			beforeLauncher, beforeStash := desktopiso.Here(home), stashLoad()
			if _, err := SetDesktopIsolated(home, enable); err == nil {
				t.Fatal("changed isolation with an unreadable profile")
			}
			if *enabled == enable || !maps.Equal(beforeUsual, desktopTree(t, usual)) ||
				!maps.Equal(beforeIsolated, desktopTree(t, isolated)) ||
				!maps.Equal(beforeStash, stashLoad()) || beforeLauncher != desktopiso.Here(home) {
				t.Fatal("failed isolation edit changed the original state")
			}
			desktopWrite(t, broken, string(original))
			if _, err := SetDesktopIsolated(home, enable); err != nil {
				t.Fatalf("could not retry after repairing the profile: %v", err)
			}
			if *enabled != enable || desktopWired(usual) == enable || desktopWired(isolated) != enable {
				t.Fatal("retry did not move the wiring")
			}
		})
	}
}

func TestClaudeDesktopIsolationKeepsWiringOnLauncherError(t *testing.T) {
	home, usual, isolated, enabled := isolatedSandbox(t)
	if err := claudeDesktop(home).Field("provider").Set("magpie"); err != nil {
		t.Fatal(err)
	}
	launcher, err := SetDesktopIsolated(home, true)
	if err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(launcher, "Contents", "Info.plist")
	if err := os.Rename(info, info+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(info, 0o755); err != nil {
		t.Fatal(err)
	}
	saved, beforeUsual, beforeIsolated := stashLoad(), desktopTree(t, usual), desktopTree(t, isolated)
	if _, err := SetDesktopIsolated(home, false); err == nil {
		t.Fatal("accepted an unreadable launcher")
	}
	if !*enabled || !maps.Equal(saved, stashLoad()) || !maps.Equal(beforeUsual, desktopTree(t, usual)) ||
		!maps.Equal(beforeIsolated, desktopTree(t, isolated)) {
		t.Fatal("launcher error changed wiring or settings")
	}
}

func TestClaudeDesktopCodeFolderRollback(t *testing.T) {
	for _, scenario := range []struct {
		name                   string
		launcher, on, failSave bool
	}{
		{"launcher failure", true, true, false},
		{"save after launcher staging", true, true, true},
		{"save before creating a launcher", false, true, true},
		{"disable keeps Code files", true, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			home, usual, isolated, _ := isolatedSandbox(t)
			if err := saveDesktopIsolatedClaude(!scenario.on, true); err != nil {
				t.Fatal(err)
			}
			codeDir := desktopIsolatedClaudeDir(home)
			codeSettings := filepath.Join(codeDir, "settings.json")
			const userSettings = `{"theme":"dark"}`
			if !scenario.on {
				desktopWrite(t, codeSettings, userSettings)
			}
			if scenario.launcher {
				if _, err := SetDesktopIsolated(home, true); err != nil {
					t.Fatal(err)
				}
			}
			launcher, saved := desktopiso.Here(home), stashLoad()
			beforeUsual, beforeIsolated := desktopTree(t, usual), desktopTree(t, isolated)
			sentinel := errors.New("cannot complete Code folder change")
			add, save := desktopIsoAdd, saveDesktopIsolatedClaude
			t.Cleanup(func() { desktopIsoAdd, saveDesktopIsolatedClaude = add, save })
			staged := false
			desktopIsoAdd = func(_, _, _, dir string, commit func() error) (string, error) {
				staged = true
				want := ""
				if scenario.on {
					want = codeDir
				}
				if dir != want || commit == nil {
					t.Errorf("launcher rebuild has Code folder %q and commit callback %v", dir, commit != nil)
				}
				if scenario.failSave && commit != nil {
					return "", commit()
				}
				return "", sentinel
			}
			if scenario.failSave {
				saveDesktopIsolatedClaude = func(bool, bool) error { return sentinel }
			}
			if err := SetDesktopIsolatedClaude(home, scenario.on); !errors.Is(err, sentinel) {
				t.Fatalf("Code folder toggle = %v, want failure", err)
			}
			if staged != scenario.launcher || desktopiso.Here(home) != launcher ||
				desktopIsolatedClaudeOn() == scenario.on || !maps.Equal(saved, stashLoad()) ||
				!maps.Equal(beforeUsual, desktopTree(t, usual)) || !maps.Equal(beforeIsolated, desktopTree(t, isolated)) {
				t.Error("failed change altered the choice, launcher or Desktop profiles")
			}
			if scenario.on {
				if _, err := os.Stat(codeDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed change left a Code folder: %v", err)
				}
			} else if body, err := os.ReadFile(codeSettings); err != nil || string(body) != userSettings {
				t.Fatalf("failed change altered the user's Code settings: %v", err)
			}
		})
	}
}

func TestClaudeDesktopGatewayKeepsPickOnProfileConflict(t *testing.T) {
	home, paths := desktopSandbox(t)
	target := "https://magpie.example.test"
	t.Setenv("MAGPIE_PUBLIC_URL", target)
	if err := access.ConfigureLAN(true, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = access.ConfigureLAN(false, false) })
	row := claudeDesktop(home)
	if err := row.Field("provider").Set("magpie"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetJSON(paths.meta, edit.KV{Path: "hybridPointer", Value: map[string]string{"bootstrapUrl": "https://config.example.test"}}); err != nil {
		t.Fatal(err)
	}
	before := desktopTree(t, paths)
	pick := settings.Shared().DesktopGateway
	if err := row.Field("gateway").Set(target); err == nil || !strings.Contains(err.Error(), "hybridPointer") {
		t.Fatalf("gateway pick accepted conflicting profile: %v", err)
	}
	if !maps.Equal(before, desktopTree(t, paths)) || settings.Shared().DesktopGateway != pick {
		t.Fatal("failed gateway pick changed the profile or stored gateway")
	}
}

func TestClaudeDesktopRestoresImportedGatewayFields(t *testing.T) {
	for _, changed := range []bool{false, true} {
		home, paths := desktopSandbox(t)
		row := claudeDesktop(home)
		set := row.Field("provider").Set
		if err := set("magpie"); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(paths.library, "imported.json")
		desktopWrite(t, other, `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"`+gateway.URL()+`","inferenceGatewayApiKey":"mk-imported"}`)
		if err := edit.SetJSON(paths.meta, edit.KV{Path: "appliedId", Value: "imported"}); err != nil {
			t.Fatal(err)
		}
		if err := row.Sync(); err != nil {
			t.Fatal(err)
		}
		if value, _ := edit.GetJSON(other, "inferenceGatewayApiKey"); value != desktopKey(desktopGateway()) {
			t.Fatal("the imported profile was not repaired")
		}
		updates := []edit.KV{{Path: "userUpdated", Value: true}}
		want := "mk-imported"
		if changed {
			want = "mk-user-changed"
			updates = append(updates, edit.KV{Path: "inferenceGatewayApiKey", Value: want})
		}
		if err := edit.SetJSON(other, updates...); err != nil {
			t.Fatal(err)
		}
		if err := set(""); err != nil {
			t.Fatal(err)
		}
		config := desktopJSON(t, other)
		if config["inferenceGatewayApiKey"] != want || config["userUpdated"] != true || config["inferenceGatewayAuthScheme"] != nil {
			t.Error("disconnect did not restore only the imported fields Magpie still owned")
		}
	}
}
