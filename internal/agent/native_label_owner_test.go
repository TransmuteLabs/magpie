package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestNativeLabelGuardAllowsInstalledSDKDeclaration(t *testing.T) {
	const label = "Fixture worker pool"
	installAgentActionFixture(t, sdk.AgentDescriptor{
		ID: "fixture-tui-label", Name: "TUI label fixture", Bin: "fixture-editor",
		ConfigFiles: []string{".fixture/config.json"},
		Fields:      []sdk.AgentField{{Key: "model", Label: label}},
	})
	actual, err := Find("fixture-tui-label")
	if err != nil || len(actual.Fields) != 1 || actual.Fields[0].Label != label || actual.extensionMetadata == nil {
		t.Fatalf("ordinary SDK declaration missing: agent=%+v error=%v", actual, err)
	}
	if !slices.ContainsFunc(All(), func(row *Agent) bool { return row.ID == actual.ID && row.Fields[0].Label == label }) {
		t.Fatal("ordinary All hid the legal SDK label")
	}
	t.Run("NativeStaticOracle", TestEveryFieldLabelHasTUIZh)
}

func TestNativeLabelGuardRejectsUnknownNativeLabel(t *testing.T) {
	home, cfg := homes()
	native := builtins(home, cfg)
	index := slices.IndexFunc(native, func(row *Agent) bool { return row.ID == "codex" && len(row.Fields) > 0 })
	if index < 0 {
		t.Fatal("native catalog did not contain field labels")
	}
	copyOf := *native[index]
	copyOf.Fields = slices.Clone(copyOf.Fields)
	copyOf.Fields[0].Label = "Untranslated native fixture field"
	native[index] = &copyOf
	zh, err := os.ReadFile("../tui/i18n.go")
	if err != nil {
		t.Fatal(err)
	}
	missing, count := untranslatedNativeAgentFields(native, nil, zh)
	if count < 30 || !slices.ContainsFunc(missing, func(message string) bool {
		return strings.Contains(message, copyOf.ID+"'s field") && strings.Contains(message, copyOf.Fields[0].Label)
	}) {
		t.Fatalf("native static oracle lost its unknown label: count=%d missing=%v", count, missing)
	}
}

func TestNativeLabelGuardSurvivesActualSDKTakeover(t *testing.T) {
	descriptor := extensionObservationDescriptor()
	descriptor.Fields[0].Label = "Fixture takeover field"
	_, home := launchAgentFixture(t, descriptor)
	path := filepath.Join(home, ".fixture/config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"personal"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := profile.Disconnect(); err != nil {
			t.Error(err)
		}
	})
	active, err := Find("codex")
	if err != nil || active.ID != descriptor.ID {
		t.Fatalf("ordinary native takeover did not occur: agent=%+v error=%v", active, err)
	}
	home, cfg := homes()
	if !slices.ContainsFunc(builtins(home, cfg), func(row *Agent) bool { return row.ID == "codex" }) {
		t.Fatal("native source catalog lost its adopted base")
	}
	t.Run("NativeStaticOracle", TestEveryFieldLabelHasTUIZh)
	t.Run("NativeUnknownOracle", TestNativeLabelGuardRejectsUnknownNativeLabel)
}
