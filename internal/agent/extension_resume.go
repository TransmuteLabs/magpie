package agent

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/update"
)

type ExtensionResumeStart struct {
	Starting Starting
	check    func(context.Context) error
}

func PrepareExtensionResume(ctx context.Context, plugin, id string) (*ExtensionResumeStart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	selected := Starter(id)
	if selected == nil || selected.ID != id || selected.extensionMetadata == nil ||
		selected.extensionMetadata.plugin != plugin || selected.extensionMetadata.descriptor.Launch == nil {
		return nil, errors.New("session resume requires its own SDK agent launch profile")
	}
	prepared, err := PrepareExtensionStart(ctx, selected)
	if err != nil {
		return nil, err
	}
	if !prepared.Starting.Active {
		return nil, errors.New("agent must be connected through its SDK profile")
	}
	return prepared, nil
}

func PrepareExtensionStart(ctx context.Context, selected *Agent) (*ExtensionResumeStart, error) {
	metadata := selected.extensionMetadata
	if metadata == nil || metadata.descriptor.Launch == nil {
		return nil, nil
	}
	home, _ := homes()
	controller := newExtensionController(home, nil, ExtensionAgent{Plugin: metadata.plugin, Agent: metadata.descriptor.Clone()}, metadata.base)
	controller.bin = selected.Bin
	before, err := controller.read(ctx)
	if err != nil {
		return nil, err
	}
	plugin, id := metadata.plugin, metadata.descriptor.ID
	start := selected.Start()
	if start.Err != nil {
		return nil, start.Err
	}
	if start.Bin == "" || start.Bin != selected.Bin ||
		start.Active && (before.Phase != "owned" || before.Bin != start.Bin) ||
		!start.Active && before.Phase != "" && before.Phase != "disconnected" {
		return nil, extension.ErrActionChanged
	}
	var owner *extensionAgentRuntime
	var address, key string
	if start.Active {
		address = gateway.URL()
		key = agentKeyAt(id, address)
		owner = &extensionAgentRuntime{manager: extension.Default(),
			entry: extension.Installed{Revision: before.PackageRevision}, config: before.ConfigVersion}
	}
	prepared := &ExtensionResumeStart{Starting: start}
	prepared.Starting.Env = slices.Clone(start.Env)
	prepared.Starting.Args = slices.Clone(start.Args)
	prepared.Starting.Plain = slices.Clone(start.Plain)
	prepared.check = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if owner != nil {
			if err := owner.check(ctx, plugin); err != nil {
				return err
			}
		}
		actual := prepared.Starting
		if actual.Err != nil || actual.Bin != start.Bin || actual.Active != start.Active ||
			actual.Model != start.Model || actual.Flag != start.Flag ||
			!slices.Equal(actual.Env, start.Env) || !slices.Equal(actual.Args, start.Args) ||
			!slices.Equal(actual.Plain, start.Plain) {
			return extension.ErrActionChanged
		}
		current, err := controller.read(ctx)
		if err != nil {
			return err
		}
		if current.Phase != before.Phase || current.Revision != before.Revision ||
			current.PackageRevision != before.PackageRevision || current.ConfigVersion != before.ConfigVersion ||
			current.Bin != before.Bin || !maps.Equal(current.Values, before.Values) ||
			!extensionBindingSameDescriptor(current.Descriptor, before.Descriptor) {
			return extension.ErrActionChanged
		}
		if !start.Active {
			return ctx.Err()
		}
		if gateway.URL() != address || agentKeyAt(id, address) != key {
			return extension.ErrActionChanged
		}
		if err := extension.ActionSnapshotsMatch(ctx, home, before.Applied); err != nil {
			return err
		}
		if err := update.ShellFunctionsMatch(before.Shells, true); err != nil {
			return err
		}
		spec := controller.spec()
		if spec == nil || spec.err != nil || !slices.Equal(spec.vars(address, key), start.Env) ||
			!slices.Equal(spec.args, start.Args) || spec.flag != start.Flag || current.Values["model"] != start.Model {
			return extension.ErrActionChanged
		}
		return ctx.Err()
	}
	return prepared, prepared.Recheck(ctx)
}

func (prepared *ExtensionResumeStart) Recheck(ctx context.Context) error {
	if prepared == nil || prepared.check == nil {
		return extension.ErrActionChanged
	}
	return prepared.check(ctx)
}
