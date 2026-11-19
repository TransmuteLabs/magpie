package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
)

func TestOrdinaryPluginCLIManagesInstalledSDK(t *testing.T) {
	host := sdkCLIHostWithAPIForTest(t, 1)
	id := host.entries[0].Manifest.ID
	output, err := runOut(t, "plugin", "list", "--json")
	var listed struct {
		SDK []extension.Installed `json:"sdk"`
	}
	if err != nil || json.Unmarshal([]byte(output), &listed) != nil ||
		len(listed.SDK) != 1 || listed.SDK[0].Manifest.ID != id {
		t.Fatalf("ordinary Installed omitted the module: %s %v", output, err)
	}
	for _, action := range []struct {
		command string
		key     string
		off     bool
	}{{"off", id, true}, {"on", "sdk:" + id, false}} {
		if output, err := runOut(t, "plugin", action.command, action.key); err != nil {
			t.Fatalf("ordinary %s: %s %v", action.command, output, err)
		}
		entries, err := extension.Default().List()
		if err != nil || len(entries) != 1 || entries[0].Disabled != action.off {
			t.Fatalf("ordinary %s saved the wrong state: %+v %v", action.command, entries, err)
		}
	}
	if output, err := runOut(t, "plugin", "update", id); err != nil || !strings.Contains(output, "local package has no update source") {
		t.Fatalf("ordinary update did not preserve local source: %s %v", output, err)
	}
	if output, err := runOut(t, "plugin", "rm", id); err != nil {
		t.Fatalf("ordinary remove: %s %v", output, err)
	}
	entries, err := extension.Default().List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("ordinary removal left a module: %+v %v", entries, err)
	}
}
