package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/update"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionController struct {
	home      string
	host      *extension.Manager
	item      ExtensionAgent
	base      *Agent
	bin       string
	root      string
	rootGuard func() error
	mu        sync.Mutex
}

type extensionAgentPlan struct {
	runtime      *extensionAgentRuntime
	action       *extension.ActionPlan
	before       extensionBindingRecord
	shell        *update.ShellFunctionPlan
	intent       string
	modelOptions json.RawMessage
	applyShell   func() error
}

func (controller *extensionController) target() string {
	if profile := controller.item.Agent.Profile; profile != nil {
		return profile.Target
	}
	return controller.item.Agent.ID
}

func (controller *extensionController) fileOnly() bool {
	return controller.item.Agent.Launch == nil && controller.item.Agent.Profile == nil
}

func (controller *extensionController) read(ctx context.Context) (extensionBindingRecord, error) {
	if err := controller.checkRoot(); err != nil {
		return extensionBindingRecord{}, err
	}
	unlock, err := extensionBindingLock(ctx, "", "ledger", true)
	if err != nil {
		return extensionBindingRecord{}, err
	}
	defer unlock()
	return controller.observe(ctx)
}

func (controller *extensionController) observe(ctx context.Context) (extensionBindingRecord, error) {
	if err := controller.checkRoot(); err != nil {
		return extensionBindingRecord{}, err
	}
	ledger, err := extensionBindingObserve(ctx)
	key := extensionBindingKey(controller.home, controller.item.Plugin, controller.item.Agent.ID)
	record := ledger.Records[key]
	if err == nil && (record.Phase == "" || record.Phase == "disconnected") {
		for otherKey, claim := range ledger.Records {
			if otherKey != key && extensionSameHome(claim.Home, controller.home) && claim.Phase != "disconnected" &&
				(extensionBindingTarget(claim) == controller.target() ||
					controller.item.Agent.Launch != nil && claim.Descriptor.Launch != nil && strings.EqualFold(controller.bin, claim.Bin)) {
				return record, fmt.Errorf("agent target is owned by %s/%s; disconnect that descriptor first", claim.Plugin, claim.Agent)
			}
		}
	}
	if err == nil && record.Revision == 0 {
		record.legacy, err = extensionLegacyAgentOwner(controller.item.Plugin, controller.item.Agent.ID)
		if err == nil && record.legacy != "" {
			if !controller.fileOnly() {
				err = fmt.Errorf("agent is owned by previous config descriptor %s/%s; reinstall its previous module release and disconnect that descriptor before adopting SDK 1.5 launch/profile wiring", controller.item.Plugin, controller.item.Agent.ID)
			} else if err = json.Unmarshal([]byte(record.legacy), &record.Before); err != nil {
				err = fmt.Errorf("extension agent ownership could not be read: %w", err)
			}
		}
	}
	if err == nil && record.Phase != "" && record.Phase != "disconnected" &&
		!extensionBindingSameDescriptor(record.Descriptor, controller.item.Agent) {
		return record, extension.ErrActionChanged
	}
	return record, err
}

func (controller *extensionController) checkRoot() error {
	if controller.rootGuard != nil {
		if err := controller.rootGuard(); err != nil {
			return err
		}
	}
	if controller.root != "" && controller.root != extensionBindingPath() {
		return errors.New("agent configuration root changed; rediscover the agent")
	}
	home, _ := homes()
	if !extensionSameHome(controller.home, home) {
		return errors.New("agent home changed; rediscover the agent")
	}
	return nil
}

func (controller *extensionController) snapshot(record extensionBindingRecord) *sdk.AgentBindingSnapshot {
	values := maps.Clone(record.Values)
	if values == nil || record.Phase == "disconnected" && !extensionBindingSameDescriptor(record.Descriptor, controller.item.Agent) {
		values = map[string]string{}
	}
	return &sdk.AgentBindingSnapshot{Target: controller.target(), Revision: strconv.FormatUint(record.Revision, 10),
		Connected: record.Phase == "owned", Values: values}
}

func (controller *extensionController) policy(record extensionBindingRecord) extension.ActionPolicy {
	policy := extension.ActionPolicy{Root: controller.home, Paths: controller.item.Agent.ConfigFiles,
		BackupDir: filepath.Join(appdir.Config(), "extensions", "action-backups", controller.item.Plugin, controller.item.Agent.ID)}
	if !controller.fileOnly() {
		policy.Binding = controller.snapshot(record)
	}
	return policy
}

func (controller *extensionController) options(record extensionBindingRecord, manifest sdk.Manifest, field, value string) ([]byte, error) {
	id, address := controller.item.Agent.ID, gateway.URL()
	options := extensionAgentOptions{Gateway: address, Key: agentKeyAt(id, address),
		Models: viaMagpie(id, "magpie/"), Field: field, Value: value}
	if record.hasOwner() {
		options.Previous = record.Before
	}
	if !controller.fileOnly() {
		options.PriorModels = record.ModelOptions
	}
	return marshalExtensionAgentOptions(options, manifest, id)
}

func (controller *extensionController) spec() *launchSpec {
	desc := controller.item.Agent
	if desc.Launch == nil {
		return nil
	}
	config := ""
	if len(desc.ConfigFiles) != 0 {
		config = filepath.Join(controller.home, filepath.FromSlash(desc.ConfigFiles[0]))
	} else if controller.base != nil && sdkLaunchUsesConfig(*desc.Launch) {
		config = controller.base.Path
	}
	return sdkLaunch(desc.ID, controller.bin, config, *desc.Launch)
}

func (controller *extensionController) prepare(ctx context.Context, intent, field, value string) (*extensionAgentPlan, error) {
	record, err := controller.read(ctx)
	if err != nil {
		return nil, err
	}
	resumeDisconnect := intent == "disconnect" && controller.fileOnly() && record.Phase == "pending" &&
		!record.RecoverySafe && !record.FinishDisconnect
	if record.Phase == "pending" && !resumeDisconnect {
		return nil, errors.New("agent transition is incomplete; disconnect to recover it")
	}
	if (intent == "sync" || intent == "disconnect") && !record.hasOwner() {
		return nil, extension.ErrActionChanged
	}
	if record.Phase == "owned" && !extensionBindingSameDescriptor(record.Descriptor, controller.item.Agent) {
		return nil, errors.New("agent definition changed while owned; disconnect before adopting its replacement")
	}
	host, err := acquireAgentRuntime(ctx, controller.host, controller.item.Plugin, controller.item.Agent)
	if err != nil {
		return nil, err
	}
	defer host.lease.Close()
	if resumeDisconnect && (host.entry.Revision != record.PackageRevision || host.config != record.ConfigVersion) {
		return nil, errors.New("agent module changed since the interrupted transition")
	}
	opts, err := controller.options(record, host.entry.Manifest, field, value)
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Models json.RawMessage `json:"models"`
	}
	if !controller.fileOnly() && host.entry.Manifest.Contract.Version.Minor >= 9 {
		if err := json.Unmarshal(opts, &catalog); err != nil {
			return nil, err
		}
	}
	action, err := extension.NewActions(host).AgentPlan(ctx, controller.item.Plugin, sdk.Scope{Operation: "agent." + intent},
		sdk.AgentPlanRequest{ID: controller.item.Agent.ID, Action: intent, Options: opts}, controller.policy(record))
	if err != nil {
		return nil, err
	}
	if resumeDisconnect {
		for _, current := range action.Snapshots() {
			matches := func(known extension.ActionSettingFile) bool {
				return current.Path == known.Path && current.BeforeHash == known.BeforeHash &&
					current.Mode == known.Mode && current.Exists == known.Exists
			}
			if !slices.ContainsFunc(record.Applied, matches) && !slices.ContainsFunc(record.PriorApplied, matches) {
				return nil, extension.ErrActionChanged
			}
		}
	}
	var shell *update.ShellFunctionPlan
	if launch := controller.spec(); launch != nil {
		if launch.err != nil {
			return nil, launch.err
		}
		var body update.ShellBody
		if action.Preview().Binding.Connected {
			body = launch.function
		} else if record.Phase == "owned" {
			body = func(kind, _ string) string {
				for _, original := range record.Shells {
					if original.Kind == kind {
						return original.Before
					}
				}
				return ""
			}
		}
		shell, err = update.PlanShellFunction(controller.bin, body)
		if err != nil {
			return nil, err
		}
	}
	return &extensionAgentPlan{runtime: host, action: action, before: record, shell: shell, intent: intent, modelOptions: catalog.Models}, nil
}

func (controller *extensionController) apply(ctx context.Context, plan *extensionAgentPlan) error {
	if err := controller.checkRoot(); err != nil {
		return err
	}
	unlock, err := extensionBindingLock(ctx, controller.home, controller.target(), false)
	if err != nil {
		return err
	}
	defer unlock()
	release, err := extensionBindingLock(ctx, "", "ledger", false)
	if err != nil {
		return err
	}
	defer release()
	ledger, err := extensionBindingRead()
	if err != nil {
		return err
	}
	key := extensionBindingKey(controller.home, controller.item.Plugin, controller.item.Agent.ID)
	prior := ledger.Records[key]
	if prior.Revision != plan.before.Revision || prior.Phase != plan.before.Phase ||
		prior.Revision == ^uint64(0) {
		return extension.ErrActionChanged
	}
	if prior.Revision == 0 {
		legacy, err := extensionLegacyAgentOwner(controller.item.Plugin, controller.item.Agent.ID)
		if err != nil {
			return err
		}
		if legacy != plan.before.legacy {
			return extension.ErrActionChanged
		}
	}
	for otherKey, claim := range ledger.Records {
		if otherKey != key && extensionSameHome(claim.Home, controller.home) && claim.Phase != "disconnected" &&
			extensionBindingTarget(claim) == controller.target() {
			return errors.New("agent target is already owned by another extension profile")
		}
	}
	if err := plan.runtime.check(ctx, controller.item.Plugin); err != nil {
		return err
	}
	snapshots := plan.action.Snapshots()
	if err := extension.ActionSnapshotsMatch(ctx, controller.home, snapshots); err != nil {
		return err
	}
	if plan.shell != nil {
		if err := update.ShellFunctionsMatch(plan.shell.Snapshots(), false); err != nil {
			return err
		}
	}
	if prior.Phase == "owned" {
		if err := update.ShellFunctionsMatch(prior.Shells, true); err != nil {
			return err
		}
	}
	if controller.unchangedSync(prior, plan) {
		return controller.applyUnchangedSync(ctx, prior, plan)
	}
	preview := plan.action.Preview()
	values, connected := map[string]string{}, plan.intent != "disconnect"
	if preview.Binding != nil {
		values, connected = maps.Clone(preview.Binding.Values), preview.Binding.Connected
	}
	record := extensionBindingRecord{
		Plugin: controller.item.Plugin, Agent: controller.item.Agent.ID, Home: controller.home,
		Revision: prior.Revision + 1, Phase: "pending", Descriptor: controller.item.Agent.Clone(), Bin: controller.bin,
		PackageRevision: plan.runtime.entry.Revision, ConfigVersion: plan.runtime.config,
		Values: values, Before: snapshots,
		ModelOptions: append(json.RawMessage(nil), plan.modelOptions...),
		Applied:      plan.action.PlannedSnapshots(), RecoverySafe: true,
	}
	if prior.hasOwner() {
		record.Before, record.PriorApplied = extension.CloneActionSnapshots(prior.Before), extension.CloneActionSnapshots(prior.Applied)
		record.PriorShells = slices.Clone(prior.Shells)
		record.RecoverySafe = prior.RecoverySafe && extension.ActionSnapshotsMatch(ctx, controller.home, prior.Applied) == nil
	} else if plan.before.legacy != "" {
		record.Before = extension.CloneActionSnapshots(plan.before.Before)
		record.RecoverySafe = false
	}
	if controller.fileOnly() && plan.before.hasOwner() {
		record.PriorApplied = snapshots
		record.FinishDisconnect = !connected
	}
	if plan.shell != nil {
		record.Shells = plan.shell.Snapshots()
		if prior.Phase == "owned" {
			for index := range record.Shells {
				for _, original := range prior.Shells {
					if original.Path == record.Shells[index].Path {
						record.Shells[index].Before, record.Shells[index].Created = original.Before, original.Created
					}
				}
			}
		}
	}
	ledger.Records[key] = record
	if err := extensionBindingWrite(ledger); err != nil {
		return err
	}
	shellApplied := false
	result, err := plan.action.ExecuteAgent(ctx, preview.Revision, func() error {
		if err := plan.runtime.check(ctx, controller.item.Plugin); err != nil {
			return err
		}
		if plan.shell == nil {
			return nil
		}
		var err error
		if plan.applyShell != nil {
			err = plan.applyShell()
		} else {
			_, err = plan.shell.Apply(plan.shell.Revision())
		}
		shellApplied = err == nil
		if err != nil {
			return err
		}
		if err := update.ShellFunctionsMatch(plan.shell.Snapshots(), true); err != nil {
			return err
		}
		return plan.runtime.check(ctx, controller.item.Plugin)
	})
	if err != nil {
		var restore error
		if plan.shell != nil {
			if shellApplied {
				restore = errors.Join(restore, update.RestoreShellFunctions(plan.shell.Snapshots()))
			}
			restore = errors.Join(restore, update.ShellFunctionsMatch(plan.shell.Snapshots(), false))
		}
		if result.RollbackIncomplete || restore != nil {
			return fmt.Errorf("agent rollback is incomplete; ownership remains pending for explicit disconnect recovery: %w", errors.Join(err, restore))
		}
		if prior.Revision == 0 {
			delete(ledger.Records, key)
		} else {
			ledger.Records[key] = prior
		}
		return errors.Join(err, extensionBindingWrite(ledger))
	}
	if !result.Applied {
		return errors.New("agent action was not applied; ownership remains pending")
	}
	record.Phase = "owned"
	if !connected {
		record.Phase = "disconnected"
	}
	record.PriorApplied = nil
	record.PriorShells = nil
	record.FinishDisconnect = false
	ledger.Records[key] = record
	if err := extensionBindingWrite(ledger); err != nil {
		return fmt.Errorf("agent action applied; ownership could not be finalized (backup %s): %w", result.Backup, err)
	}
	return nil
}

func (controller *extensionController) recover(ctx context.Context, revision uint64) error {
	if err := controller.checkRoot(); err != nil {
		return err
	}
	unlock, err := extensionBindingLock(ctx, controller.home, controller.target(), false)
	if err != nil {
		return err
	}
	defer unlock()
	release, err := extensionBindingLock(ctx, "", "ledger", false)
	if err != nil {
		return err
	}
	defer release()
	ledger, err := extensionBindingRead()
	if err != nil {
		return err
	}
	key := extensionBindingKey(controller.home, controller.item.Plugin, controller.item.Agent.ID)
	record := ledger.Records[key]
	if record.Revision != revision {
		return extension.ErrActionChanged
	}
	if record.Phase == "" || record.Phase == "disconnected" {
		return nil
	}
	if !extensionBindingSameDescriptor(record.Descriptor, controller.item.Agent) {
		return extension.ErrActionChanged
	}
	if !record.RecoverySafe && !record.FinishDisconnect {
		return errors.New("agent files contain rebased user edits; enable the original module to disconnect safely")
	}
	if record.Revision == ^uint64(0) {
		return errors.New("agent binding revision exhausted")
	}
	if err := update.ShellFunctionsRecoverable(record.Shells, record.PriorShells); err != nil {
		return err
	}
	original, known := record.Before, [][]extension.ActionSettingFile{record.Applied, record.PriorApplied}
	if record.FinishDisconnect {
		original, known = record.Applied, [][]extension.ActionSettingFile{record.PriorApplied}
	}
	if err := extension.RestoreOwnedActionSnapshots(ctx, controller.home, original, known...); err != nil {
		return err
	}
	if err := update.RestoreShellFunctions(record.Shells, record.PriorShells); err != nil {
		return err
	}
	record.Revision++
	record.Phase = "disconnected"
	record.PriorApplied = nil
	record.PriorShells = nil
	record.FinishDisconnect = false
	ledger.Records[key] = record
	return extensionBindingWrite(ledger)
}

// newExtensionController keeps inherited launch metadata identical for ordinary
// discovery, source-backed resume and owned-process resolution.
func newExtensionController(home string, host *extension.Manager, item ExtensionAgent, base *Agent) *extensionController {
	controller := &extensionController{home: home, host: host, item: item, base: base, bin: item.Agent.Bin, root: extensionBindingPath(),
		rootGuard: extensionAgentRootGuard(home)}
	if controller.bin == "" && base != nil {
		controller.bin = base.Bin
	}
	return controller
}

func extensionControlledAgent(home string, host *extension.Manager, item ExtensionAgent, base *Agent) *Agent {
	controller := newExtensionController(home, host, item, base)
	desc := item.Agent
	a := &Agent{ID: desc.ID, Name: desc.Name, Bin: controller.bin, Aliases: slices.Clone(desc.Aliases), ListOnly: true, Spelled: prefixed}
	if len(desc.ConfigFiles) != 0 {
		a.Path = filepath.Join(home, filepath.FromSlash(desc.ConfigFiles[0]))
		a.Dir = filepath.Dir(a.Path)
	}
	for _, prefix := range desc.UA {
		a.UA = append(a.UA, strings.ToLower(prefix))
	}
	read := func() (extensionBindingRecord, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return controller.observe(ctx)
	}
	owned := func() bool {
		record, err := read()
		return err == nil && record.hasOwner()
	}
	a.extensionMetadata = &extensionAgentMetadata{plugin: item.Plugin, descriptor: desc.Clone(), base: base, owns: owned,
		disconnected: func() bool {
			record, err := read()
			return err == nil && (record.Phase == "disconnected" || controller.fileOnly() &&
				record.Revision == 0 && !record.hasOwner() && stashShared()[desc.ID+disconnectedKey] == "1")
		}}
	a.detect = func() bool {
		for _, name := range desc.ConfigFiles {
			if isFile(filepath.Join(home, filepath.FromSlash(name))) {
				return true
			}
		}
		return owned() || base != nil && base.Detected() || controller.bin != "" && lookExtensionBin(controller.bin)
	}
	run := func(intent, key, value string) error {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if intent == "sync" {
			record, err := controller.observe(ctx)
			if err != nil {
				return err
			}
			if !record.hasOwner() {
				return nil
			}
		}
		plan, err := controller.prepare(ctx, intent, key, value)
		if err != nil {
			return err
		}
		return controller.apply(ctx, plan)
	}
	a.Native = &NativeConnection{
		Read: func() NativeState {
			record, err := read()
			state := NativeState{Provider: "disconnected", Runtime: "available", Fields: map[string]NativeField{}}
			values, connected := record.Values, record.Phase == "owned"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err == nil && record.Phase != "pending" && (controller.fileOnly() || record.hasOwner()) {
				runtime, runtimeErr := acquireAgentRuntime(ctx, host, item.Plugin, desc)
				err = runtimeErr
				if runtime != nil {
					if !controller.fileOnly() && (runtime.entry.Revision != record.PackageRevision || runtime.config != record.ConfigVersion) {
						err = errors.New("agent module changed; sync or reconnect before using it")
					} else {
						opts, optionsErr := controller.options(record, runtime.entry.Manifest, "", "")
						err = optionsErr
						if err == nil {
							inspection, inspectErr := extension.NewActions(runtime).AgentInspect(ctx, item.Plugin, sdk.Scope{Operation: "agent.inspect"},
								sdk.AgentInspectRequest{ID: desc.ID, Options: opts}, controller.policy(record))
							state.Detail, err = inspection.Detail, inspectErr
							if controller.fileOnly() {
								values, connected = inspection.Values, inspection.Connected && record.hasOwner()
							}
						}
					}
					runtime.lease.Close()
				}
			}
			if record.Phase == "pending" {
				state.NeedsRecovery = err == nil
				err = errors.Join(err, errors.New("agent binding needs recovery"))
			}
			if err != nil {
				state.Provider, state.Runtime, state.Detail = "invalid", "unavailable", err.Error()
			} else if connected {
				state.Provider = "connected"
			}
			for _, field := range desc.Fields {
				state.Fields[field.Key] = NativeField{Value: values[field.Key], Status: state.Provider}
			}
			return state
		},
		Connect: func() error { return run("connect", "", "") },
		Apply: func(key, value string) error {
			if controller.fileOnly() && a.Field(key) == nil {
				return fmt.Errorf("unknown extension agent field %q", key)
			}
			return run("set", key, value)
		},
	}
	var pending *extensionAgentPlan
	var revision string
	a.Native.Disconnect = func() (*DisconnectPlan, error) {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		pending, revision = nil, ""
		record, err := controller.read(ctx)
		if err != nil {
			return nil, err
		}
		result := &DisconnectPlan{}
		if !record.hasOwner() {
			return result, nil
		}
		pending, err = controller.prepare(ctx, "disconnect", "", "")
		if err != nil {
			if record.legacy != "" {
				return nil, err
			}
			if controller.fileOnly() && record.Phase == "owned" {
				if _, ownerErr := inspectAgentRuntime(ctx, host, item.Plugin, desc); ownerErr == nil {
					return nil, err
				}
			}
			pending = nil
			result.actionRevision = "recover:" + strconv.FormatUint(record.Revision, 10)
		} else {
			result.actionRevision = pending.action.Preview().Revision
			for _, change := range pending.action.Preview().Files {
				for _, before := range pending.action.Snapshots() {
					if before.Path == change.Path {
						result.Files = append(result.Files, PlannedFile{Path: filepath.Join(home, filepath.FromSlash(change.Path)), Before: before.Body, After: change.Body})
					}
				}
			}
			if pending.shell != nil {
				result.actionRevision += ":" + pending.shell.Revision()
				changes, err := pending.shell.Preview()
				if err != nil {
					return nil, err
				}
				for _, change := range changes {
					result.Files = append(result.Files, PlannedFile{Path: change.Path, Before: change.Before, After: change.After})
				}
			}
		}
		revision = result.Revision(desc.ID)
		return result, nil
	}
	a.Native.Execute = func(plan *DisconnectPlan) error {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		if plan == nil {
			return extension.ErrActionChanged
		}
		if revision != plan.Revision(desc.ID) {
			record, err := read()
			if err != nil {
				return err
			}
			if !record.hasOwner() && len(plan.Files) == 0 && len(plan.Settings) == 0 {
				return nil
			}
			return extension.ErrActionChanged
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if pending != nil {
			return controller.apply(ctx, pending)
		}
		record, err := controller.read(ctx)
		if err != nil || plan.actionRevision != "recover:"+strconv.FormatUint(record.Revision, 10) {
			return extension.ErrActionChanged
		}
		return controller.recover(ctx, record.Revision)
	}
	a.Native.ExecuteOffline = a.Native.Execute
	a.Sync = func() error { return run("sync", "", "") }
	for _, field := range desc.Fields {
		key := field.Key
		a.Fields = append(a.Fields, Field{Key: key, Label: field.Label,
			Get: func() string {
				if controller.fileOnly() {
					return a.Native.Read().Fields[key].Value
				}
				record, _ := read()
				return record.Values[key]
			},
			Set: func(value string) error { return a.Native.Apply(key, value) },
			Options: func(current map[string]string) []Option {
				options := viaMagpie(desc.ID, "magpie/")
				if !controller.fileOnly() {
					return options
				}
				picks := []Option{{Value: "", Label: "default"}}
				if value := current[key]; value != "" {
					picks = append(picks, Option{Value: value, Label: value})
				}
				return append(picks, options...)
			},
		})
	}
	if launch := controller.spec(); launch != nil {
		if a.Path == "" {
			a.extensionMetadata.inheritedLaunchConfig = launch.config
		}
		a.Start = func() Starting {
			if launch.err != nil {
				return Starting{Bin: controller.bin, Err: launch.err}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			record, err := controller.read(ctx)
			start := Starting{Bin: controller.bin, Plain: slices.Clone(launch.plain)}
			if err != nil {
				start.Err = err
				return start
			}
			if record.Phase == "" || record.Phase == "disconnected" {
				return start
			}
			host, err := acquireAgentRuntime(ctx, controller.host, item.Plugin, desc)
			if host != nil {
				if host.entry.Revision != record.PackageRevision || host.config != record.ConfigVersion {
					err = errors.New("agent module changed; sync or reconnect before launching it")
				}
				host.lease.Close()
			}
			if err == nil && record.Phase != "owned" {
				err = errors.New("agent binding transition is incomplete")
			}
			if err == nil {
				err = extension.ActionSnapshotsMatch(ctx, home, record.Applied)
			}
			if err == nil {
				err = update.ShellFunctionsMatch(record.Shells, true)
			}
			start.Err = err
			if err == nil {
				start.Active, start.Env, start.Args = true, launch.vars(gateway.URL(), agentKeyAt(desc.ID, gateway.URL())), slices.Clone(launch.args)
				start.Model, start.Flag = record.Values["model"], launch.flag
			}
			return start
		}
		a.Launch = func() string { return "magpie " + desc.ID + " run" }
		a.Typed = func() string {
			if owned() {
				return controller.bin
			}
			return ""
		}
	}
	return a
}
