package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	runtime    *extensionAgentRuntime
	action     *extension.ActionPlan
	before     extensionBindingRecord
	shell      *update.ShellFunctionPlan
	intent     string
	applyShell func() error
}

func (controller *extensionController) target() string {
	if profile := controller.item.Agent.Profile; profile != nil {
		return profile.Target
	}
	return controller.item.Agent.ID
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
		var legacy bool
		legacy, err = extensionLegacyAgentOwned(controller.item.Plugin, controller.item.Agent.ID)
		if err == nil && legacy {
			err = fmt.Errorf("agent is owned by previous config descriptor %s/%s; reinstall its previous module release and disconnect that descriptor before adopting SDK 1.5 launch/profile wiring", controller.item.Plugin, controller.item.Agent.ID)
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
	return extension.ActionPolicy{Root: controller.home, Paths: controller.item.Agent.ConfigFiles,
		Binding:   controller.snapshot(record),
		BackupDir: filepath.Join(appdir.Config(), "extensions", "action-backups", controller.item.Plugin, controller.item.Agent.ID)}
}

func (controller *extensionController) spec() *launchSpec {
	desc := controller.item.Agent
	if desc.Launch == nil {
		return nil
	}
	config := ""
	if len(desc.ConfigFiles) != 0 {
		config = filepath.Join(controller.home, filepath.FromSlash(desc.ConfigFiles[0]))
	} else if controller.base != nil {
		config = controller.base.Path
	}
	launch := declaredLaunch(desc.ID, controller.bin, config, desc.Launch.Model, desc.Launch.Env, desc.Launch.Args, desc.Launch.Plain)
	launch.strict = true
	if config == "" {
		for _, templates := range []map[string]string{desc.Launch.Env, desc.Launch.Args} {
			for _, value := range templates {
				if strings.Contains(value, "{config}") {
					launch.err = errors.New("declared agent launch requires an available configuration path")
				}
			}
		}
	}
	return launch
}

func (controller *extensionController) prepare(ctx context.Context, intent, field, value string) (*extensionAgentPlan, error) {
	record, err := controller.read(ctx)
	if err != nil {
		return nil, err
	}
	if record.Phase == "pending" {
		return nil, errors.New("agent transition is incomplete; disconnect to recover it")
	}
	if record.Phase == "owned" && !extensionBindingSameDescriptor(record.Descriptor, controller.item.Agent) {
		return nil, errors.New("agent definition changed while owned; disconnect before adopting its replacement")
	}
	host, err := acquireAgentRuntime(ctx, controller.host, controller.item.Plugin, controller.item.Agent)
	if err != nil {
		return nil, err
	}
	defer host.lease.Close()
	var previous []extension.ActionSettingFile
	if record.Phase == "owned" {
		previous = record.Before
	}
	opts, err := json.Marshal(extensionAgentOptions{Gateway: gateway.URL(), Key: agentKeyAt(controller.item.Agent.ID, gateway.URL()),
		Models: viaMagpie(controller.item.Agent.ID, "magpie/"), Field: field, Value: value, Previous: previous})
	if err != nil {
		return nil, err
	}
	action, err := extension.NewActions(host).AgentPlan(ctx, controller.item.Plugin, sdk.Scope{Operation: "agent." + intent},
		sdk.AgentPlanRequest{ID: controller.item.Agent.ID, Action: intent, Options: opts}, controller.policy(record))
	if err != nil {
		return nil, err
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
	return &extensionAgentPlan{runtime: host, action: action, before: record, shell: shell, intent: intent}, nil
}

func extensionAppliedSnapshots(plan *extension.ActionPlan) []extension.ActionSettingFile {
	snapshots := plan.Snapshots()
	for _, change := range plan.Preview().Files {
		for index := range snapshots {
			if snapshots[index].Path != change.Path {
				continue
			}
			file := &snapshots[index]
			file.Body, file.Exists = slices.Clone(change.Body), !change.Remove
			if !file.Exists {
				file.Mode, file.BeforeHash = 0, extension.MissingActionHash
			} else {
				if change.Mode != 0 {
					file.Mode = change.Mode
				} else if file.Mode == 0 {
					file.Mode = 0o600
				}
				digest := sha256.Sum256(file.Body)
				file.BeforeHash = hex.EncodeToString(digest[:])
			}
		}
	}
	return snapshots
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
	for otherKey, claim := range ledger.Records {
		if otherKey != key && extensionSameHome(claim.Home, controller.home) && claim.Phase != "disconnected" &&
			extensionBindingTarget(claim) == controller.target() {
			return errors.New("agent target is already owned by another extension profile")
		}
	}
	if err := plan.runtime.check(ctx, controller.item.Plugin); err != nil {
		return err
	}
	if err := extension.ActionSnapshotsMatch(ctx, controller.home, plan.action.Snapshots()); err != nil {
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
	record := extensionBindingRecord{
		Plugin: controller.item.Plugin, Agent: controller.item.Agent.ID, Home: controller.home,
		Revision: prior.Revision + 1, Phase: "pending", Descriptor: controller.item.Agent.Clone(), Bin: controller.bin,
		PackageRevision: plan.runtime.entry.Revision, ConfigVersion: plan.runtime.config,
		Values: maps.Clone(plan.action.Preview().Binding.Values), Before: plan.action.Snapshots(),
		Applied: extensionAppliedSnapshots(plan.action), RecoverySafe: true,
	}
	if prior.Phase == "owned" {
		record.Before, record.PriorApplied = extension.CloneActionSnapshots(prior.Before), extension.CloneActionSnapshots(prior.Applied)
		record.PriorShells = slices.Clone(prior.Shells)
		record.RecoverySafe = prior.RecoverySafe && extension.ActionSnapshotsMatch(ctx, controller.home, prior.Applied) == nil
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
	result, err := plan.action.Execute(ctx, plan.action.Preview().Revision)
	shellApplied := false
	if err == nil && plan.shell != nil {
		err = plan.runtime.check(ctx, controller.item.Plugin)
		if err == nil && plan.applyShell != nil {
			err = plan.applyShell()
		} else if err == nil {
			_, err = plan.shell.Apply(plan.shell.Revision())
		}
		shellApplied = err == nil
	}
	if err == nil {
		err = plan.runtime.check(ctx, controller.item.Plugin)
	}
	if err != nil {
		restore := extension.RestoreOwnedActionSnapshots(context.WithoutCancel(ctx), controller.home, plan.action.Snapshots(), record.Applied)
		if plan.shell != nil {
			if shellApplied {
				restore = errors.Join(restore, update.RestoreShellFunctions(plan.shell.Snapshots()))
			}
			restore = errors.Join(restore, update.ShellFunctionsMatch(plan.shell.Snapshots(), false))
		}
		if restore != nil {
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
	if !plan.action.Preview().Binding.Connected {
		record.Phase = "disconnected"
	}
	record.PriorApplied = nil
	record.PriorShells = nil
	ledger.Records[key] = record
	return extensionBindingWrite(ledger)
}

func (controller *extensionController) recover(ctx context.Context) error {
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
	if record.Phase == "" || record.Phase == "disconnected" {
		return nil
	}
	if !extensionBindingSameDescriptor(record.Descriptor, controller.item.Agent) {
		return extension.ErrActionChanged
	}
	if !record.RecoverySafe {
		return errors.New("agent files contain rebased user edits; enable the original module to disconnect safely")
	}
	if record.Revision == ^uint64(0) {
		return errors.New("agent binding revision exhausted")
	}
	if err := update.ShellFunctionsRecoverable(record.Shells, record.PriorShells); err != nil {
		return err
	}
	if err := extension.RestoreOwnedActionSnapshots(ctx, controller.home, record.Before, record.Applied, record.PriorApplied); err != nil {
		return err
	}
	if err := update.RestoreShellFunctions(record.Shells, record.PriorShells); err != nil {
		return err
	}
	record.Revision++
	record.Phase = "disconnected"
	ledger.Records[key] = record
	return extensionBindingWrite(ledger)
}

func extensionControlledAgent(home string, host *extension.Manager, item ExtensionAgent, base *Agent) *Agent {
	controller := &extensionController{home: home, host: host, item: item, base: base, bin: item.Agent.Bin, root: extensionBindingPath(),
		rootGuard: extensionAgentRootGuard(home)}
	desc := item.Agent
	if controller.bin == "" && base != nil {
		controller.bin = base.Bin
	}
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
		return err == nil && record.Phase != "" && record.Phase != "disconnected"
	}
	a.extensionMetadata = &extensionAgentMetadata{plugin: item.Plugin, descriptor: desc.Clone(), base: base, owns: owned,
		disconnected: func() bool { record, err := read(); return err == nil && record.Phase == "disconnected" }}
	a.detect = func() bool {
		return owned() || base != nil && base.Detected() || controller.bin != "" && lookExtensionBin(controller.bin) || a.Path != "" && isFile(a.Path)
	}
	run := func(intent, key, value string) error {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if intent == "sync" && !owned() {
			return nil
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
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err == nil && owned() {
				runtime, runtimeErr := acquireAgentRuntime(ctx, host, item.Plugin, desc)
				err = runtimeErr
				if runtime != nil {
					if runtime.entry.Revision != record.PackageRevision || runtime.config != record.ConfigVersion {
						err = errors.New("agent module changed; sync or reconnect before using it")
					} else {
						inspection, inspectErr := extension.NewActions(runtime).AgentInspect(ctx, item.Plugin, sdk.Scope{Operation: "agent.inspect"},
							sdk.AgentInspectRequest{ID: desc.ID}, controller.policy(record))
						state.Detail, err = inspection.Detail, inspectErr
					}
					runtime.lease.Close()
				}
			}
			if err != nil || record.Phase == "pending" {
				state.Provider, state.Runtime, state.Detail = "invalid", "unavailable", fmt.Sprint(errors.Join(err, errors.New("agent binding needs recovery")))
			} else if record.Phase == "owned" {
				state.Provider = "connected"
			}
			for _, field := range desc.Fields {
				state.Fields[field.Key] = NativeField{Value: record.Values[field.Key], Status: state.Provider}
			}
			return state
		},
		Connect: func() error { return run("connect", "", "") },
		Apply:   func(key, value string) error { return run("set", key, value) },
	}
	var pending *extensionAgentPlan
	var revision string
	a.Native.Disconnect = func() (*DisconnectPlan, error) {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		record, err := controller.read(ctx)
		if err != nil {
			return nil, err
		}
		result := &DisconnectPlan{}
		if record.Phase == "" || record.Phase == "disconnected" {
			return result, nil
		}
		pending, err = controller.prepare(ctx, "disconnect", "", "")
		if err != nil {
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
		if revision != plan.Revision(desc.ID) {
			if !owned() && len(plan.Files) == 0 {
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
		return controller.recover(ctx)
	}
	a.Native.ExecuteOffline = a.Native.Execute
	a.Sync = func() error { return run("sync", "", "") }
	for _, field := range desc.Fields {
		key := field.Key
		a.Fields = append(a.Fields, Field{Key: key, Label: field.Label,
			Get:     func() string { record, _ := read(); return record.Values[key] },
			Set:     func(value string) error { return run("set", key, value) },
			Options: func(_ map[string]string) []Option { return viaMagpie(desc.ID, "magpie/") },
		})
	}
	if launch := controller.spec(); launch != nil {
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
