package agent

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/update"
)

func (controller *extensionController) unchangedSync(prior extensionBindingRecord, plan *extensionAgentPlan) bool {
	if plan.intent != "sync" || prior.Phase != "owned" || !prior.RecoverySafe ||
		plan.applyShell != nil ||
		!bytes.Equal(prior.ModelOptions, plan.modelOptions) ||
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
