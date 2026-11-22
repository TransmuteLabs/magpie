package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModuleConfigEqualityMatchesLegacy(t *testing.T) {
	for _, test := range []struct {
		name, left, right string
		want              bool
	}{
		{"nested reorder", `{"object":{"a":1,"b":true},"array":[null,"s"]}`, `{"array":[null,"s"],"object":{"b":true,"a":1}}`, true},
		{"number spelling", `{"a":1}`, `{"a":1.0}`, false},
		{"number kind", `{"a":1}`, `{"a":"1"}`, false},
		{"array order", `{"a":[1,2]}`, `{"a":[2,1]}`, false},
		{"nested type", `{"a":[]}`, `{"a":{}}`, false},
		{"missing null key", `{"a":null}`, `{"b":null}`, false},
		{"null object", `null`, ` null `, true},
		{"null differs from object", `null`, `{}`, false},
		{"malformed", `{"a":1}`, `{"a":}`, false},
		{"legacy first value", `{"a":1}`, `{"a":1} true`, true},
		{"legacy last duplicate", `{"a":1}`, `{"a":2,"a":1}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			left := sdk.ConfigSnapshot{Version: "1", Values: json.RawMessage(test.left)}
			right := sdk.ConfigSnapshot{Version: "1", Values: json.RawMessage(test.right)}
			if got := sameModuleConfig(left, right); got != test.want {
				t.Fatalf("equality = %t, want %t", got, test.want)
			}
			if got := sameModuleConfig(right, left); got != test.want {
				t.Fatalf("reversed equality = %t, want %t", got, test.want)
			}
		})
	}
	if !sameModuleConfig(sdk.ConfigSnapshot{Version: "1"}, sdk.ConfigSnapshot{Version: "1"}) {
		t.Fatal("version-only equality was rejected")
	}
	if sameModuleConfig(sdk.ConfigSnapshot{Version: "1"}, sdk.ConfigSnapshot{Version: "2"}) {
		t.Fatal("version-only equality accepted a different version")
	}
}

func TestRuntimeConfigReorderedEqualityGuardsPublication(t *testing.T) {
	manager, entry := runtimeConfigFixture(t, 0)
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	ctx, id := t.Context(), entry.Manifest.ID
	before, reordered := moduleConfigEqualityFixture(128, "fixture")
	if _, err := manager.CompareAndSwapModuleConfig(ctx, id, "1", before.Values); err != nil {
		t.Fatal(err)
	}
	expected, err := manager.runtimeModuleConfigContext(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}

	publications := 0
	check := func(want bool) {
		t.Helper()
		previous := publications
		err := manager.withActiveConfig(ctx, entry, expected, func() error {
			publications++
			return nil
		})
		if (err == nil) != want || (publications == previous+1) != want {
			t.Fatalf("allowed = %t, publications = %d, error = %v", want, publications-previous, err)
		}
	}
	check(true)
	replaceRuntimeConfigAtSameVersion(t, manager, id, string(before.Values), string(reordered.Values))
	current, err := manager.runtimeModuleConfigContext(ctx, entry)
	if err != nil || current.Version != expected.Version || bytes.Equal(current.Values, expected.Values) {
		t.Fatalf("invalid reordered fixture: version=%q, byte equality=%t, error=%v", current.Version, bytes.Equal(current.Values, expected.Values), err)
	}
	check(true)
	check(true)

	replaceRuntimeConfigAtSameVersion(t, manager, id, `"mode":"fast"`, `"mode":"safe"`)
	check(false)
	replaceRuntimeConfigAtSameVersion(t, manager, id, `"mode":"safe"`, `"mode"!"fast"`)
	check(false)
	replaceRuntimeConfigAtSameVersion(t, manager, id, `"mode"!"fast"`, `"mode":"fast"`)
	check(true)
}

func moduleConfigEqualityFixture(fields int, label string) (sdk.ConfigSnapshot, sdk.ConfigSnapshot) {
	var extras strings.Builder
	for index := range fields {
		fmt.Fprintf(&extras, `,"private%03d":{"flag":true,"text":"0123456789ABCDEF","value":%d}`, index, index)
	}
	left := fmt.Sprintf(`{"mode":"fast","label":%q%s}`, label, extras.String())
	right := fmt.Sprintf(`{"label":%q,"mode":"fast"%s}`, label, extras.String())
	return sdk.ConfigSnapshot{Version: "1", Values: json.RawMessage(left)},
		sdk.ConfigSnapshot{Version: "1", Values: json.RawMessage(right)}
}
