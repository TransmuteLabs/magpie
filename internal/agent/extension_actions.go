package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type ExtensionAgent struct {
	Plugin string              `json:"plugin"`
	Agent  sdk.AgentDescriptor `json:"agent"`
}

type ExtensionAgentRegistry interface {
	extension.ActionRuntime
	List() ([]extension.Installed, error)
}

var _ ExtensionAgentRegistry = (*extension.Manager)(nil)

func ExtensionAgents(host ExtensionAgentRegistry) ([]ExtensionAgent, error) {
	installed, err := host.List()
	if err != nil {
		return nil, err
	}
	agents := []ExtensionAgent{}
	owners := map[string]string{}
	for _, entry := range installed {
		if entry.Disabled || !entry.Manifest.Contract.Has(sdk.AgentWiring) {
			continue
		}
		for _, descriptor := range entry.Manifest.Agents {
			if owner, exists := owners[descriptor.ID]; exists {
				return nil, fmt.Errorf("extension agent %s has owners %s and %s", descriptor.ID, owner, entry.Manifest.ID)
			}
			owners[descriptor.ID] = entry.Manifest.ID
			agents = append(agents, ExtensionAgent{Plugin: entry.Manifest.ID, Agent: descriptor})
		}
	}
	return agents, nil
}

func PlanExtension(ctx context.Context, host extension.ActionRuntime, plugin string, scope sdk.Scope, request sdk.AgentPlanRequest, policy extension.ActionPolicy) (*extension.ActionPlan, error) {
	return extension.NewActions(host).AgentPlan(ctx, plugin, scope, request, policy)
}

// extensionAgents connects executable SDK modules to the same ordinary
// Agent/NativeConnection API the UI and agent commands already use.
func extensionAgents(home string, existing []*Agent, errs map[string]string) []*Agent {
	host := extension.Default()
	list, err := ExtensionAgents(host)
	if err != nil {
		if errs != nil {
			errs["sdk"] = err.Error()
		}
		return nil
	}
	taken := map[string]bool{magpieID: true}
	for _, a := range append(append([]*Agent{}, existing...), others...) {
		taken[strings.ToLower(a.ID)] = true
		for _, alias := range a.Aliases {
			taken[strings.ToLower(alias)] = true
		}
	}
	var out []*Agent
	for _, item := range list {
		if len(item.Agent.ConfigFiles) == 0 {
			continue // ID/Name-only modules have the explicit action API.
		}
		if taken[strings.ToLower(item.Agent.ID)] {
			if errs != nil {
				errs[item.Plugin] = fmt.Sprintf("agent id %q is already owned", item.Agent.ID)
			}
			continue
		}
		taken[strings.ToLower(item.Agent.ID)] = true
		out = append(out, extensionAgent(home, host, item))
	}
	return out
}

// Settings before adoption are host-owned. A plan receives them as opaque
// snapshots so the module, which knows its config schema, can restore only
// its own keys and preserve subsequent user edits.
type extensionAgentOptions struct {
	Gateway  string                        `json:"gateway"`
	Key      string                        `json:"key"`
	Models   []Option                      `json:"models"`
	Field    string                        `json:"field,omitempty"`
	Value    string                        `json:"value,omitempty"`
	Previous []extension.ActionSettingFile `json:"previous,omitempty"`
}

func extensionAgent(home string, host ExtensionAgentRegistry, item ExtensionAgent) *Agent {
	desc := item.Agent
	actions := extension.NewActions(host)
	policy := extension.ActionPolicy{Root: home, Paths: desc.ConfigFiles,
		BackupDir: filepath.Join(appdir.Config(), "extensions", "action-backups", item.Plugin, desc.ID)}
	ownerKey := "sdk.agent." + item.Plugin + ":" + desc.ID
	var mu sync.Mutex
	owned := func() bool { return stashShared()[ownerKey] != "" }
	options := func(field, value string) ([]byte, error) {
		opts := extensionAgentOptions{Gateway: gateway.URL(), Key: agentKeyAt(desc.ID, gateway.URL()),
			Models: viaMagpie(desc.ID, "magpie/"), Field: field, Value: value}
		if raw := stashShared()[ownerKey]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &opts.Previous); err != nil {
				return nil, fmt.Errorf("extension agent ownership could not be read: %w", err)
			}
		}
		return json.Marshal(opts)
	}
	inspect := func() (sdk.AgentInspectResult, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		opts, err := options("", "")
		if err != nil {
			return sdk.AgentInspectResult{}, err
		}
		return actions.AgentInspect(ctx, item.Plugin, sdk.Scope{Operation: "agent.inspect"},
			sdk.AgentInspectRequest{ID: desc.ID, Options: opts}, policy)
	}
	prepare := func(action, field, value string) (*extension.ActionPlan, error) {
		opts, err := options(field, value)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return actions.AgentPlan(ctx, item.Plugin, sdk.Scope{Operation: "agent." + action},
			sdk.AgentPlanRequest{ID: desc.ID, Action: action, Options: opts}, policy)
	}
	execute := func(plan *extension.ActionPlan, disconnect bool) error {
		// Native API callers serialize each object's operations. The action
		// service also rechecks every file while holding the root write lock.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result, err := plan.Execute(ctx, plan.Preview().Revision)
		if err != nil {
			return err
		}
		stashMu.Lock()
		defer stashMu.Unlock()
		state := stashLoad()
		if disconnect {
			delete(state, ownerKey)
			state[desc.ID+disconnectedKey] = "1"
		} else {
			if state[ownerKey] == "" {
				snapshot, err := json.Marshal(plan.Snapshots())
				if err != nil {
					return err
				}
				state[ownerKey] = string(snapshot)
			}
			delete(state, desc.ID+disconnectedKey)
		}
		if err := stashWrite(state); err != nil {
			return fmt.Errorf("agent files applied; ownership could not be saved (backup %s): %w", result.Backup, err)
		}
		return nil
	}
	run := func(action, key, value string) error {
		mu.Lock()
		defer mu.Unlock()
		plan, err := prepare(action, key, value)
		if err != nil {
			return err
		}
		return execute(plan, false)
	}
	a := &Agent{ID: desc.ID, Name: desc.Name, Bin: desc.Bin, Path: filepath.Join(home, filepath.FromSlash(desc.ConfigFiles[0])),
		ListOnly: true, Spelled: prefixed}
	a.Dir = filepath.Dir(a.Path)
	a.detect = func() bool {
		for _, name := range desc.ConfigFiles {
			if isFile(filepath.Join(home, filepath.FromSlash(name))) {
				return true
			}
		}
		return desc.Bin != "" && lookExtensionBin(desc.Bin)
	}
	a.Native = &NativeConnection{
		Read: func() NativeState {
			result, err := inspect()
			state := NativeState{Provider: "disconnected", Runtime: "available", Fields: map[string]NativeField{}}
			if err != nil {
				state.Provider, state.Runtime, state.Detail = "invalid", "unavailable", err.Error()
				return state
			}
			state.Detail = result.Detail
			if result.Connected && owned() {
				state.Provider = "connected"
			}
			for _, f := range desc.Fields {
				state.Fields[f.Key] = NativeField{Value: result.Values[f.Key], Status: state.Provider}
			}
			return state
		},
		Connect: func() error { return run("connect", "", "") },
		Apply: func(key, value string) error {
			if a.Field(key) == nil {
				return fmt.Errorf("unknown extension agent field %q", key)
			}
			return run("set", key, value)
		},
	}
	var pending *extension.ActionPlan
	var pendingRevision string
	a.Native.Disconnect = func() (*DisconnectPlan, error) {
		mu.Lock()
		defer mu.Unlock()
		if !owned() {
			pending, pendingRevision = nil, ""
			return &DisconnectPlan{}, nil
		}
		plan, err := prepare("disconnect", "", "")
		if err != nil {
			return nil, err
		}
		before := map[string][]byte{}
		for _, file := range plan.Snapshots() {
			before[file.Path] = file.Body
		}
		native := &DisconnectPlan{actionRevision: plan.Preview().Revision}
		for _, file := range plan.Preview().Files {
			native.Files = append(native.Files, PlannedFile{Path: filepath.Join(plan.Root(), filepath.FromSlash(file.Path)),
				Before: before[file.Path], After: file.Body})
		}
		pending, pendingRevision = plan, native.Revision(desc.ID)
		return native, nil
	}
	a.Native.Execute = func(plan *DisconnectPlan) error {
		mu.Lock()
		defer mu.Unlock()
		if pending == nil && !owned() && len(plan.Files) == 0 && len(plan.Settings) == 0 {
			return nil
		}
		if pending == nil || pendingRevision != plan.Revision(desc.ID) {
			return extension.ErrActionChanged
		}
		return execute(pending, true)
	}
	a.Sync = func() error {
		if !owned() {
			return nil
		}
		return run("sync", "", "")
	}
	for _, f := range desc.Fields {
		key := f.Key
		a.Fields = append(a.Fields, Field{Key: key, Label: f.Label,
			Get: func() string { result, _ := inspect(); return result.Values[key] },
			Set: func(value string) error { return a.Native.Apply(key, value) },
			Options: func(cur map[string]string) []Option {
				opts := []Option{{Value: "", Label: "default"}}
				if cur[key] != "" {
					opts = append(opts, Option{Value: cur[key], Label: cur[key]})
				}
				return append(opts, viaMagpie(desc.ID, "magpie/")...)
			},
		})
	}
	return a
}

func lookExtensionBin(bin string) bool {
	_, err := proc.Look(bin)
	return err == nil
}
