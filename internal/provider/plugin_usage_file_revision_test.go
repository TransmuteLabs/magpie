package provider

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

func TestPluginUsageEditorRechecksCurrentLoginFile(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"replaced account", func(data []byte) []byte {
			return bytes.ReplaceAll(data, []byte(`"epoch-before"`), []byte(`"epoch--after"`))
		}},
		{"corrupt with backup", func(data []byte) []byte {
			return bytes.Repeat([]byte{0}, len(data))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			saved := loginOf(t, readLogins(), login.Agent, login.User)
			saved.PluginObservations.Epoch = "epoch-before"
			saveLogins(t, saved)

			path := loginsPath()
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := test.change(data)
			if len(changed) != len(data) || bytes.Equal(changed, data) {
				t.Fatal("fixture needs different contents with equal sizes")
			}
			if err := os.WriteFile(lastgood.Bak(path), data, 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			reply, seeded := startWalletEditorRead(t, ctx, login.Agent, requests)
			reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","windows":[{"name":"5 hours","used":80}]}`}
			if quota := readWalletEditorResult(t, seeded)[login.User]; len(quota.Windows) != 1 {
				t.Fatalf("could not warm the editor: %+v", quota)
			}

			release := filememo.Hold()
			defer release()
			replacement := path + ".peer"
			if err := os.WriteFile(replacement, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(replacement, old, old); err != nil {
				t.Fatal(err)
			}
			if err := steady.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			result, err := LoginUsageEditor(ctx, login.Agent)
			if err != nil {
				t.Fatal(err)
			}
			if quota := result[login.User]; quota.Provider == "" || !quota.ReadDeferred || len(quota.Windows) != 0 {
				t.Errorf("editor accepted the previous login snapshot: %+v", quota)
			}
			if len(requests) != 0 {
				t.Error("warm publication attempted a vendor read")
			}
		})
	}
}
