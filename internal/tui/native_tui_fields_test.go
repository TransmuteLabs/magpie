package tui

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestTUIFieldOwnerRejectsRegistryErrors(t *testing.T) {
	fields, err := nativeTUIFields(nil, func() ([]agent.ExtensionAgent, error) { return nil, os.ErrPermission })
	if !errors.Is(err, os.ErrPermission) || fields != nil {
		t.Fatalf("failed registry was admitted as empty: fields=%v error=%v", fields, err)
	}
}

func TestTUIFieldOwnerChecksExactDeclaredTuples(t *testing.T) {
	installed := installTUILabelFixture(t, false)
	actual, err := agent.ExtensionAgents(extension.Default())
	if err != nil || len(actual) != 1 {
		t.Fatalf("actual installed declarations: %v %v", actual, err)
	}
	for _, change := range []string{"missing", "id", "key", "label", "duplicate-agent", "duplicate-field", "matching"} {
		t.Run(change, func(t *testing.T) {
			declared := slices.Clone(actual)
			declared[0].Agent = actual[0].Agent.Clone()
			switch change {
			case "missing":
				declared = nil
			case "id":
				declared[0].Agent.ID = "other-agent"
			case "key":
				declared[0].Agent.Fields[0].Key = "other"
			case "label":
				declared[0].Agent.Fields[0].Label = "other label"
			case "duplicate-agent":
				declared = append(declared, declared[0])
			case "duplicate-field":
				declared[0].Agent.Fields = append(declared[0].Agent.Fields, declared[0].Agent.Fields[0])
			}
			fields, err := nativeTUIFields([]*agent.Agent{installed}, func() ([]agent.ExtensionAgent, error) { return declared, nil })
			if change == "matching" {
				if err != nil || len(fields) != 0 {
					t.Fatalf("matching dynamic tuple entered static native oracle: %v %v", fields, err)
				}
			} else if err == nil || fields != nil {
				t.Fatalf("unmatched SDK tuple was silently skipped: change=%s fields=%v error=%v", change, fields, err)
			}
		})
	}
}

func TestTUIFieldOwnerCannotWhitelistCoincidentNativeTuple(t *testing.T) {
	const label = "Untranslated native fixture field"
	native := &agent.Agent{ID: "fixture-native", Fields: []agent.Field{{Key: "model", Label: label}}}
	declaration := agent.ExtensionAgent{Plugin: "fixture.dynamic", Agent: sdk.AgentDescriptor{
		ID: native.ID, Fields: []sdk.AgentField{{Key: "model", Label: label}},
	}}
	fields, err := nativeTUIFields([]*agent.Agent{native}, func() ([]agent.ExtensionAgent, error) {
		return []agent.ExtensionAgent{declaration}, nil
	})
	if err != nil || len(fields) != 1 || fields[0].Label != label {
		t.Fatalf("dynamic declaration hid native label: fields=%v error=%v", fields, err)
	}
	if translated, exists := zh[label]; exists && strings.TrimSpace(translated) != "" {
		t.Fatal("unknown native negative accidentally has a translation")
	}
}

func TestTUIFieldOwnerKeepsUnknownAdoptedBaseLabel(t *testing.T) {
	profile := installTUILabelFixture(t, true)
	base, _, _ := profile.SDKLibraryTarget()
	if base == nil || len(base.Fields) == 0 {
		t.Fatal("actual adopted base has no native fields")
	}
	before := base.Fields
	base.Fields = slices.Clone(base.Fields)
	base.Fields[0].Label = "Untranslated adopted native field"
	defer func() { base.Fields = before }()
	declared, err := agent.ExtensionAgents(extension.Default())
	if err != nil {
		t.Fatal(err)
	}
	fields, err := nativeTUIFields([]*agent.Agent{profile}, func() ([]agent.ExtensionAgent, error) { return declared, nil })
	if err != nil || !slices.ContainsFunc(fields, func(field agent.Field) bool { return field.Label == base.Fields[0].Label }) {
		t.Fatalf("SDK takeover erased unknown native field: fields=%v error=%v", fields, err)
	}
	if _, exists := zh[base.Fields[0].Label]; exists {
		t.Fatal("unknown adopted native negative accidentally has a translation")
	}
}
