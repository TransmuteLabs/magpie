package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/update"
)

func (controller *extensionController) unchangedSync(prior extensionBindingRecord, plan *extensionAgentPlan) bool {
	if plan.intent != "sync" || prior.Phase != "owned" || !prior.RecoverySafe ||
		plan.applyShell != nil ||
		!sameExtensionModelAuthority(prior.ModelOptions, plan.modelOptions) ||
		prior.PackageRevision != plan.runtime.entry.Revision || prior.ConfigVersion != plan.runtime.config ||
		prior.Bin != controller.bin || !extensionBindingSameDescriptor(prior.Descriptor, controller.item.Agent) {
		return false
	}
	preview := plan.action.Preview()
	if preview.Binding == nil || !preview.Binding.Connected || !maps.Equal(prior.Values, preview.Binding.Values) ||
		len(preview.Files) != 0 {
		return false
	}
	if !slices.EqualFunc(prior.Applied, plan.action.Snapshots(), func(first, second extension.ActionSettingFile) bool {
		return first.Path == second.Path && first.BeforeHash == second.BeforeHash &&
			first.Mode == second.Mode && first.Exists == second.Exists && bytes.Equal(first.Body, second.Body)
	}) {
		return false
	}
	var shells []update.ShellFunctionSnapshot
	if plan.shell != nil {
		shells = plan.shell.Snapshots()
	}
	return slices.EqualFunc(prior.Shells, shells, func(owned, planned update.ShellFunctionSnapshot) bool {
		return owned.Path == planned.Path && owned.Kind == planned.Kind && owned.Name == planned.Name &&
			owned.After == planned.Before && owned.After == planned.After && !planned.Created
	})
}

func sameExtensionModelAuthority(first, second json.RawMessage) bool {
	if bytes.Equal(first, second) {
		return true
	}
	var catalogs [2][]map[string]json.RawMessage
	if json.Unmarshal(first, &catalogs[0]) != nil || json.Unmarshal(second, &catalogs[1]) != nil ||
		(catalogs[0] == nil) != (catalogs[1] == nil) || len(catalogs[0]) != len(catalogs[1]) {
		return false
	}
	var normalized [2][]string
	for index, models := range catalogs {
		normalized[index] = make([]string, len(models))
		for position, model := range models {
			for _, key := range []string{"label", "note", "icon", "icons", "group", "groupIcon"} {
				delete(model, key)
			}
			raw, err := json.Marshal(model)
			if err != nil {
				return false
			}
			normalized[index][position] = string(raw)
		}
		slices.Sort(normalized[index])
	}
	return slices.Equal(normalized[0], normalized[1])
}

func (controller *extensionController) applyUnchangedSync(ctx context.Context, prior extensionBindingRecord, plan *extensionAgentPlan) error {
	result, err := plan.action.Execute(ctx, plan.action.Preview().Revision)
	if err != nil {
		return err
	}
	if !result.Applied {
		return errors.New("agent synchronization did not validate its checked plan")
	}
	if err := controller.checkRoot(); err != nil {
		return err
	}
	if err := plan.runtime.check(ctx, controller.item.Plugin); err != nil {
		return err
	}
	if err := extension.ActionSnapshotsMatch(ctx, controller.home, prior.Applied); err != nil {
		return err
	}
	if err := update.ShellFunctionsMatch(prior.Shells, true); err != nil {
		return err
	}
	return ctx.Err()
}
