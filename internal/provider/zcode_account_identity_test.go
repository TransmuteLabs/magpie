package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/plugin"
)

func TestZCodeAppLabelRequiresMatchingFamilyAndUID(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode":   {"type": "oauth", "accountId": "saved A", "refresh": `{"source":"zcode","site":"zai","uid":"a"}`},
		"zcode#2": {"type": "oauth", "accountId": "saved B", "refresh": `{"source":"zcode","site":"bigmodel","uid":"a"}`},
		"zcode#3": {"type": "oauth", "accountId": "old", "refresh": `{"source":"zcode","site":"zai"}`},
		"zcode#4": {"type": "oauth", "accountId": "no family", "refresh": `{"source":"zcode","uid":"a"}`},
	})
	old := zcodeAppLabel
	t.Cleanup(func() { zcodeAppLabel = old })
	zcodeAppLabel = func() (string, string, string) { return "zai", "a", "current app" }
	pp := plugin.Provider{ID: "zcode", Accounts: []plugin.Account{{Key: "zcode"}, {Key: "zcode#2"}, {Key: "zcode#3"}, {Key: "zcode#4"}}}
	_, names := pluginOwnNames(pp)
	if len(names) != 1 || names["zcode"] != "current app" {
		t.Fatalf("labels across families or without UID: %v", names)
	}
	zcodeAppLabel = func() (string, string, string) { return "zai", "other", "different account" }
	_, names = pluginOwnNames(pp)
	if len(names) != 0 {
		t.Fatalf("different account was labelled as the app user: %v", names)
	}
}

func TestZCodeAppLabelReadsActiveNormalizedProfile(t *testing.T) {
	claudeHome(t)
	path := zcodeCredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store := map[string]string{
		"oauth:active_provider":    "bigmodel",
		"oauth:bigmodel:user_info": `{"id":"bm-uid","displayName":"BigModel user"}`,
		"oauth:zai:user_info":      `{"user_id":"zai-uid","name":"other family"}`,
	}
	b, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	family, uid, name := zcodeAppLabel()
	if family != "bigmodel" || uid != "bm-uid" || name != "BigModel user" {
		t.Fatalf("active profile = %q %q %q", family, uid, name)
	}
}
