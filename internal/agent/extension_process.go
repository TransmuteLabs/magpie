package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/update"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	extension.RegisterAgentProcessResolver(resolveOrdinaryExtensionProcess)
}

func resolveOrdinaryExtensionProcess(ctx context.Context, lease *extension.Lease, recipe sdk.AgentProcessRecipe) (extension.OwnedProcessLaunch, error) {
	home, _ := homes()
	return ResolveExtensionProcess(ctx, lease, recipe, home, 0)
}

func ResolveExtensionProcess(ctx context.Context, lease *extension.Lease, recipe sdk.AgentProcessRecipe,
	directory string, routeID int64,
) (extension.OwnedProcessLaunch, error) {
	if err := sdk.ValidateAgentProcessRecipe(recipe); err != nil {
		return extension.OwnedProcessLaunch{}, err
	}
	manifest := lease.Manifest()
	position := slices.IndexFunc(manifest.Agents, func(descriptor sdk.AgentDescriptor) bool {
		return descriptor.ID == recipe.Agent
	})
	if position < 0 || !filepath.IsAbs(directory) {
		return extension.OwnedProcessLaunch{}, errors.New("process requires its declared agent and approved directory")
	}
	home, _ := homes()
	descriptor := manifest.Agents[position]
	base := extensionProfileBase(home, descriptor, nil)
	if descriptor.Profile != nil && base == nil {
		return extension.OwnedProcessLaunch{}, errors.New("agent profile target is unavailable")
	}
	controller := newExtensionController(home, nil, ExtensionAgent{Plugin: manifest.ID, Agent: descriptor}, base)
	record, err := controller.read(ctx)
	if err != nil {
		return extension.OwnedProcessLaunch{}, err
	}
	valid := func(current extensionBindingRecord) bool {
		return current.Phase == "owned" && current.Revision != 0 && current.Revision == record.Revision &&
			current.PackageRevision == lease.Revision() && current.ConfigVersion == lease.ConfigVersion() &&
			current.Bin == record.Bin && current.Bin != "" &&
			extensionBindingSameDescriptor(current.Descriptor, descriptor)
	}
	if !valid(record) {
		return extension.OwnedProcessLaunch{}, errors.New("process agent binding is not currently owned")
	}
	executable, err := exec.LookPath(record.Bin)
	if err != nil || !filepath.IsAbs(executable) {
		return extension.OwnedProcessLaunch{}, errors.New("owned agent executable is unavailable")
	}
	controller.bin = record.Bin
	spec := controller.spec()
	arguments := slices.Clone(recipe.Argv)
	environment := make(map[string]string)
	for _, name := range []string{"PATH", "SystemRoot", "WINDIR", "PATHEXT", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ",
		"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		if value, exists := os.LookupEnv(name); exists {
			environment[name] = value
		}
	}
	environment["HOME"], environment["USERPROFILE"] = home, home
	gatewayURL := gateway.URL()
	key := ""
	if spec != nil {
		if spec.err != nil || spec.flag != "" && record.Values["model"] == "" {
			return extension.OwnedProcessLaunch{}, errors.New("owned agent launch has no selected model or valid configuration")
		}
		key = agentKeyAt(descriptor.ID, gatewayURL)
		for _, variable := range spec.vars(gatewayURL, key) {
			name, value, _ := strings.Cut(variable, "=")
			environment[name] = value
		}
		arguments = slices.Clone(spec.args)
		if spec.flag != "" {
			arguments = append(arguments, spec.flag, record.Values["model"])
		}
		arguments = append(arguments, recipe.Argv...)
	}
	recipe.Argv = arguments
	if err := sdk.ValidateAgentProcessRecipe(recipe); err != nil {
		return extension.OwnedProcessLaunch{}, err
	}
	check := func(ctx context.Context) error {
		if err := lease.CheckCurrent(ctx); err != nil {
			return err
		}
		current, err := controller.read(ctx)
		if err != nil || !valid(current) || current.Values["model"] != record.Values["model"] {
			return errors.New("owned process agent binding changed")
		}
		resolved, err := exec.LookPath(record.Bin)
		if err != nil || resolved != executable {
			return errors.New("owned process executable selection changed")
		}
		if spec != nil && (gateway.URL() != gatewayURL || agentKeyAt(descriptor.ID, gatewayURL) != key) {
			return errors.New("owned process gateway binding changed")
		}
		if err := extension.ActionSnapshotsMatch(ctx, home, record.Applied); err != nil {
			return err
		}
		return update.ShellFunctionsMatch(record.Shells, true)
	}
	if err := check(ctx); err != nil {
		return extension.OwnedProcessLaunch{}, err
	}
	watch := map[string]bool{home: true, filepath.Dir(extensionBindingPath()): true, filepath.Dir(executable): true}
	for _, file := range record.Applied {
		watch[filepath.Dir(filepath.Join(home, filepath.FromSlash(file.Path)))] = true
	}
	for _, shell := range record.Shells {
		watch[filepath.Dir(shell.Path)] = true
	}
	var paths, env []string
	for path := range watch {
		paths = append(paths, path)
	}
	for _, name := range sortedKeys(environment) {
		env = append(env, name+"="+environment[name])
	}
	return extension.OwnedProcessLaunch{
		Recipe: recipe, Executable: executable, Directory: directory, Env: env,
		Profile: descriptor.ID, Revision: strconv.FormatUint(record.Revision, 10), RouteID: routeID,
		WatchPaths: paths, Check: check,
		Admit: func(ctx context.Context) (func(), error) {
			unlock, err := extensionBindingLock(ctx, home, controller.target(), true)
			if err != nil {
				return nil, err
			}
			release, err := extensionBindingLock(ctx, "", "ledger", true)
			if err != nil {
				unlock()
				return nil, err
			}
			return func() { release(); unlock() }, nil
		},
	}, nil
}
