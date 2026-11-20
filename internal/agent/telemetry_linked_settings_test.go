package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
)

func TestTelemetryCheckedSettingsRejectConcurrentEdits(t *testing.T) {
	for _, fixture := range []struct {
		name, file, before, comment string
		open                        func(string) *Agent
	}{
		{"json", ".claude/settings.json", "{\r\n  // user comment\r\n  \"model\": \"opus\"\r\n}\r\n", "// user edited\r\n", claude},
		{"codex", ".codex/config.toml", "model = \"gpt-5.5\"\n", "# user edited\n", codex},
		{"kimi", moreTelemetry[1].file, moreTelemetry[1].before, "# user edited\n", moreTelemetry[1].of},
		{"goose", moreTelemetry[2].file, moreTelemetry[2].before, "# user edited\n", moreTelemetry[2].of},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, off := range []bool{true, false} {
				operation := "restore"
				if off {
					operation = "disable"
				}
				t.Run(operation, func(t *testing.T) {
					home, path := telemetryHome(t, fixture.file, fixture.before)
					agent := fixture.open(home)
					if !off {
						if err := agent.SetTelemetry(true); err != nil {
							t.Fatal(err)
						}
					}
					claim := strings.TrimSuffix(agent.Telemetry.all, ".all")
					before := stashLoad()[claim]
					external := fixture.comment + readFile(path)
					err := interfereWithTelemetry(t, path, external, func() error { return agent.SetTelemetry(off) })
					if !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
						t.Errorf("SetTelemetry(%v) = %v, concurrent bytes preserved = %t", off, err, readFile(path) == external)
					}
					if got := stashLoad()[claim]; got != before {
						t.Errorf("rejected operation changed telemetry claim from %q to %q", before, got)
					}
				})
			}
		})
	}
}

func TestTelemetryCheckedSettingsUsesOneClaudeWrite(t *testing.T) {
	const original = "{\r\n  // keep it\r\n  \"env\": {\r\n    \"FOO\": \"bar\"\r\n  }\r\n}\r\n"
	home, path := telemetryHome(t, ".claude/settings.json", original)
	agent := claude(home)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []bool{true, false} {
		writes := 0
		err := edit.Filtered(func(name string, proposed []byte) []byte {
			if name == target {
				writes++
			}
			return proposed
		}, func() error { return agent.SetTelemetry(off) })
		if err != nil || writes != 1 || telemetryOf(t, agent).Off != off {
			t.Errorf("SetTelemetry(%v) = %v, writes = %d", off, err, writes)
		}
	}
	if got := readFile(path); got != original {
		t.Fatalf("round trip changed JSONC: %q", got)
	}
}

func TestTelemetryCheckedSettingsPreservesBOM(t *testing.T) {
	for _, fixture := range []struct {
		name, file, body string
		open             func(string) *Agent
	}{
		{"codex", ".codex/config.toml", "[analytics]\r\nextra = true\r\n", codex},
		{"kimi", moreTelemetry[1].file, moreTelemetry[1].before, moreTelemetry[1].of},
		{"goose", moreTelemetry[2].file, moreTelemetry[2].before, moreTelemetry[2].of},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			const bom = "\uFEFF"
			original := bom + fixture.body
			home, path := telemetryHome(t, fixture.file, original)
			agent := fixture.open(home)
			if err := agent.SetTelemetry(true); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(readFile(path), bom) || !telemetryOf(t, agent).Off {
				t.Fatal("telemetry stayed on or the BOM was removed")
			}
			if err := agent.SetTelemetry(false); err != nil {
				t.Fatal(err)
			}
			if got := readFile(path); got != original {
				t.Fatalf("round trip changed settings: %q", got)
			}
		})
	}
}

func TestTelemetryCheckedSettingsGlobalRetry(t *testing.T) {
	home, path := telemetryHome(t, ".claude/settings.json", `{"env":{"FOO":"bar"}}`)
	onPath(t, "claude")
	agent := claude(home)
	claim := strings.TrimSuffix(agent.Telemetry.all, ".all")
	external := `{"env":{"FOO":"baz"}}`
	err := interfereWithTelemetry(t, path, external, func() error { return SetTelemetryAll(true) })
	if !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
		t.Errorf("global disable = %v, concurrent bytes preserved = %t", err, readFile(path) == external)
	}
	if saved := stashLoad(); saved[claim] != "" || saved[agent.Telemetry.all] != "" || !settings.Load().AgentTelemetryOff {
		t.Error("global disable changed agent ownership or lost requested global setting")
	}
	if t.Failed() {
		return
	}
	if err := SetTelemetryAll(true); err != nil {
		t.Fatal(err)
	}
	before := stashLoad()[claim]
	external = `{"env":{"FOO":"baz","DISABLE_TELEMETRY":"","DISABLE_ERROR_REPORTING":"1"}}`
	err = interfereWithTelemetry(t, path, external, func() error { return SetTelemetryAll(false) })
	if !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
		t.Errorf("global restore = %v, concurrent bytes preserved = %t", err, readFile(path) == external)
	}
	if saved := stashLoad(); saved[claim] != before || saved[agent.Telemetry.all] != "1" || settings.Load().AgentTelemetryOff {
		t.Error("global restore forgot the pending agent cleanup or lost requested global setting")
	}
	if t.Failed() {
		return
	}
	if err := SetTelemetryAll(false); err != nil {
		t.Fatal(err)
	}
	if got, exists := jsonAt(t, path, "env.DISABLE_TELEMETRY"); !exists || got != "" {
		t.Error("retry changed the user's telemetry value")
	}
	if _, exists := jsonAt(t, path, "env.DISABLE_ERROR_REPORTING"); exists {
		t.Error("retry left the owned telemetry value")
	}
	if saved := stashLoad(); saved[claim] != "" || saved[agent.Telemetry.all] != "" {
		t.Error("successful global restore retained ownership")
	}
}

func TestTelemetryCheckedSettingsGlobalRestoreLeavesNewUserValues(t *testing.T) {
	home, path := telemetryHome(t, ".claude/settings.json", `{}`)
	onPath(t, "claude")
	if err := SetTelemetryAll(true); err != nil {
		t.Fatal(err)
	}
	agent := claude(home)
	claim := strings.TrimSuffix(agent.Telemetry.all, ".all")
	if stashLoad()[agent.Telemetry.all] != "1" {
		t.Fatal("global switch did not claim the initial write")
	}
	const theirs = `{"env":{"DISABLE_TELEMETRY":"yes","DISABLE_ERROR_REPORTING":"yes"}}`
	writeFile(t, path, theirs)
	if err := SetTelemetryAll(false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(path); got != theirs {
		t.Fatalf("global restore changed the user's values: %q", got)
	}
	if saved := stashLoad(); saved[claim] != "" || saved[agent.Telemetry.all] != "" {
		t.Fatal("successful global restore retained ownership")
	}
}

func interfereWithTelemetry(t *testing.T, path, external string, operation func() error) error {
	t.Helper()
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name == target && !edited {
			edited = true
			writeFile(t, name, external)
		}
		return proposed
	}, operation)
	if !edited {
		t.Fatal("telemetry write was not reached")
	}
	return err
}
