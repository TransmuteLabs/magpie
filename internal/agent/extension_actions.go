package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/extension"
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
	return listExtensionAgents(host.List())
}

func listExtensionAgents(installed []extension.Installed, err error) ([]ExtensionAgent, error) {
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
	return extensionAgentsContext(context.Background(), home, existing, errs)
}

func extensionAgentsContext(parent context.Context, home string, existing []*Agent, errs map[string]string) []*Agent {
	host := extension.Default()
	out := installedExtensionAgentsContext(parent, host, home, existing, errs)
	if _, err := os.Lstat(extensionBindingPath()); os.IsNotExist(err) {
		return out
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ledger, err := extensionBindingObserve(ctx)
	if err != nil {
		if errs != nil {
			errs["sdk-ownership"] = err.Error()
		}
		return out
	}
	for _, record := range ledger.Records {
		if !extensionSameHome(record.Home, home) || record.Phase == "disconnected" {
			continue
		}
		if slices.ContainsFunc(out, func(a *Agent) bool {
			return a.ID == record.Agent && a.extensionMetadata != nil && a.extensionMetadata.plugin == record.Plugin &&
				extensionBindingSameDescriptor(a.extensionMetadata.descriptor, record.Descriptor)
		}) {
			continue
		}
		out = slices.DeleteFunc(out, func(a *Agent) bool { return a.ID == record.Agent })
		base := extensionProfileBase(home, record.Descriptor, existing)
		out = append(out, extensionControlledAgent(home, host, ExtensionAgent{Plugin: record.Plugin, Agent: record.Descriptor.Clone()}, base))
	}
	return out
}

func installedExtensionAgents(host *extension.Manager, home string, existing []*Agent, errs map[string]string) []*Agent {
	return installedExtensionAgentsContext(context.Background(), host, home, existing, errs)
}

func installedExtensionAgentsContext(ctx context.Context, host *extension.Manager, home string, existing []*Agent, errs map[string]string) []*Agent {
	list, err := listExtensionAgents(host.ListContext(ctx))
	if err != nil {
		if errs != nil {
			errs["sdk"] = err.Error()
		}
		list = nil
	}
	taken := map[string]bool{magpieID: true}
	binaries := map[string]bool{}
	for _, a := range append(append([]*Agent{}, existing...), others...) {
		taken[strings.ToLower(a.ID)] = true
		for _, alias := range a.Aliases {
			taken[strings.ToLower(alias)] = true
		}
		if a.Bin != "" {
			binaries[strings.ToLower(a.Bin)] = true
		}
	}
	var out []*Agent
	for _, item := range list {
		if len(item.Agent.ConfigFiles) == 0 && item.Agent.Launch == nil {
			continue // ID/Name-only modules have the explicit action API.
		}
		base := extensionProfileBase(home, item.Agent, existing)
		if item.Agent.Profile != nil && base == nil {
			if errs != nil {
				errs[item.Plugin] = "agent profile target is unavailable"
			}
			continue
		}
		conflict := ""
		for _, identity := range append([]string{item.Agent.ID}, item.Agent.Aliases...) {
			if taken[strings.ToLower(identity)] {
				conflict = identity
				break
			}
		}
		if conflict != "" {
			if errs != nil {
				errs[item.Plugin] = fmt.Sprintf("agent identity %q is already owned", conflict)
			}
			continue
		}
		bin := item.Agent.Bin
		if bin == "" && base != nil {
			bin = base.Bin
		}
		if item.Agent.Launch != nil && binaries[strings.ToLower(bin)] && (base == nil || base.Bin != bin) {
			if errs != nil {
				errs[item.Plugin] = fmt.Sprintf("agent launch command %q is already owned; declare an explicit profile", bin)
			}
			continue
		}
		taken[strings.ToLower(item.Agent.ID)] = true
		for _, alias := range item.Agent.Aliases {
			taken[strings.ToLower(alias)] = true
		}
		if item.Agent.Launch != nil {
			binaries[strings.ToLower(bin)] = true
		}
		out = append(out, extensionControlledAgent(home, host, item, base))
	}
	return out
}

// extensionProfileBase resolves only ordinary local targets. Process callers
// have no discovery list, so build that list without re-entering SDK discovery
// or asking WSL to start. Profiles may inherit built-in or legacy plugin paths.
func extensionProfileBase(home string, descriptor sdk.AgentDescriptor, existing []*Agent) *Agent {
	if descriptor.Profile == nil {
		return nil
	}
	if existing == nil {
		_, cfg := homes()
		own := onMachine(home, cfg)
		existing = append(own, pluggedAgents(home, own, nil)...)
	}
	for _, candidate := range existing {
		if candidate.ID == descriptor.Profile.Target && candidate.extensionMetadata == nil && candidate.WSL == "" {
			return candidate
		}
	}
	return nil
}

// Settings before adoption are host-owned. A plan receives them as opaque
// snapshots so the module, which knows its config schema, can restore only
// its own keys and preserve subsequent user edits.
type extensionAgentOptions struct {
	Gateway     string                        `json:"gateway"`
	Key         string                        `json:"key"`
	Models      []Option                      `json:"models"`
	Field       string                        `json:"field,omitempty"`
	Value       string                        `json:"value,omitempty"`
	Previous    []extension.ActionSettingFile `json:"previous,omitempty"`
	PriorModels json.RawMessage               `json:"priorModels,omitempty"`
}

func marshalExtensionAgentOptions(options extensionAgentOptions, manifest sdk.Manifest, agent string) ([]byte, error) {
	if manifest.Contract.Version.Minor < 9 {
		options.PriorModels = nil
		for index := range options.Models {
			options.Models[index].MaxOutput, options.Models[index].DefaultEffort = 0, ""
			options.Models[index].Efforts = nil
		}
		return json.Marshal(options)
	}
	decisions, err := extensionAgentDecisionModels(agent, manifest)
	if err != nil {
		return nil, err
	}
	var models []json.RawMessage
	if options.Models != nil {
		models = make([]json.RawMessage, len(options.Models))
	}
	for index, option := range options.Models {
		if err := sdk.ValidateAgentModelMetadata(manifest, sdk.AgentModelMetadata{MaxOutput: int64(option.MaxOutput),
			Efforts: option.Efforts, DefaultEffort: option.DefaultEffort}); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(struct {
			Option
			Efforts []string `json:"efforts"`
		}{option, option.Efforts})
		if err != nil {
			return nil, err
		}
		models[index] = raw
	}
	type envelope extensionAgentOptions
	return json.Marshal(struct {
		envelope
		Models         []json.RawMessage        `json:"models"`
		DecisionModels []sdk.AgentDecisionModel `json:"decisionModels"`
	}{envelope(options), models, decisions})
}

func lookExtensionBin(bin string) bool {
	_, err := proc.Look(bin)
	return err == nil
}
