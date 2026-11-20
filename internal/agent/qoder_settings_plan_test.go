package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQoderModelPickPreservesConcurrentProviderEdit(t *testing.T) {
	for _, build := range []qoderBuild{qoderGlobal, qoderChina} {
		t.Run(build.id, func(t *testing.T) {
			home := syncHome(t)
			t.Setenv(build.env, "")
			at := here(home)
			path := filepath.Join(home, build.dir, "settings.json")
			before := `{"providers":{"magpie":{"displayName":"before"}},"model":{"name":"performance","reasoningEffort":"low"}}`
			edited := `{"providers":{"magpie":{"displayName":"user edit","routing":{"summary":"native"}}},"model":{"name":"ultimate","reasoningEffort":"max"}}`
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			editedDuringPick := false
			at.base = func() string {
				if !editedDuringPick {
					editedDuringPick = true
					if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return "http://127.0.0.1:3425"
			}
			agent := qoderAt(at, build)
			err := agent.Apply("model", "magpie/relay/glm-4.6")
			if raw, readErr := os.ReadFile(path); readErr != nil || string(raw) != edited {
				t.Errorf("model pick overwrote the concurrent edit: %s (%v)", raw, readErr)
			}
			if !editedDuringPick || err == nil {
				t.Errorf("model pick accepted changed settings (edited %t): %v", editedDuringPick, err)
			}
			if saved := peek(build.id + ":" + path + ":model"); len(saved) != 0 {
				t.Errorf("rejected model pick left a recovery record: %v", saved)
			}
		})
	}
}
