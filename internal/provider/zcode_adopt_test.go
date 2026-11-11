package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

func TestZCodeIntrinsicAdoptsOwnAndTeamWithoutTouchingOtherAccounts(t *testing.T) {
	home := claudeHome(t)
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "test-secret")
	credentials := filepath.Join(home, ".zcode", "v2", "credentials.json")
	writeFile(t, credentials, map[string]any{
		"oauth:zai:user_info": zcodeEncrypt(t, `{"user_id":"own-user","email":"own@example.test"}`),
		"zcodejwttoken":       zcodeEncrypt(t, zcodeTestJWT(time.Now().Add(time.Hour))),
	})
	team := zcodeKey{Base: ZCodeBigModelBase, Token: "team-business", Org: "org", Project: "project", Key: "team.key", UID: "family-user"}
	saveLogins(t,
		savedLogin{Agent: "zcode", User: "team@example.test", Plan: "Team", On: true, Auth: json.RawMessage(jsonText(team))},
		savedLogin{Agent: "unrelated", User: "other", On: true, Auth: json.RawMessage(`{"token":"other-secret"}`)},
	)
	writeFile(t, plugin.ListPath(), plugin.List{Plugins: []plugin.Entry{
		{Spec: "@magpie-community/opencode-zcode-auth@0.1.58", Options: map[string]any{"freeFirst": false}},
		{Spec: "other-plugin", Options: map[string]any{"keep": true}},
	}})
	writeFile(t, plugin.AuthPath(), map[string]any{"unrelated": map[string]any{"type": "api", "key": "other-secret"}})
	if err := settings.EditPluginOptions(func(options map[string]map[string]bool) (map[string]map[string]bool, error) {
		options["other"] = map[string]bool{"keep": true}
		return options, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := adoptZCodeAccounts(); err != nil {
		t.Fatal(err)
	}
	if Movable("zcode") || !PluginServes("zcode") {
		t.Fatal("ZCode remained owned by its retired mover")
	}
	auth := plugin.Auths("zcode")
	if len(auth) != 2 {
		t.Fatalf("expected discovered app and saved team slots, got %d", len(auth))
	}
	seenOwn, seenTeam := false, false
	for _, a := range auth {
		var state map[string]any
		if json.Unmarshal([]byte(a["refresh"].(string)), &state) != nil {
			t.Fatal("unreadable adopted state")
		}
		switch a["accountId"] {
		case "own@example.test":
			seenOwn = state["source"] == "zcode" && state["uid"] == nil
		case "team@example.test":
			seenTeam = state["site"] == "bigmodel" && state["org"] == "org" && state["project"] == "project" && state["key"] == "team.key" && state["token"] == "team-business"
		}
	}
	if !seenOwn || !seenTeam {
		t.Fatalf("app/team material not adopted: own=%v team=%v", seenOwn, seenTeam)
	}
	if got := plugin.Auths("unrelated")["unrelated"]["key"]; got != "other-secret" {
		t.Fatalf("unrelated credential changed: %v", got)
	}
	if options := settings.Load().PluginOptions; options["zcode"]["free_first"] || !options["other"]["keep"] {
		t.Fatalf("options not preserved: %+v", options)
	}
	if _, err := os.Stat(credentials); err != nil {
		t.Fatalf("app credentials changed: %v", err)
	}
	if len(plugin.Load().Plugins) != 2 {
		t.Fatalf("external plugin list changed: %+v", plugin.Load())
	}
	if err := adoptZCodeAccounts(); err != nil || len(plugin.Auths("zcode")) != 2 {
		t.Fatalf("adoption was not idempotent: %v", err)
	}
}
