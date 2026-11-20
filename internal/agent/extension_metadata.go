package agent

import (
	"context"
	"strings"

	"github.com/yetone/magpie/internal/agentplug"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func HelpBin(a *Agent, all []*Agent) string {
	if a.Bin != "" || a.WSL == "" {
		return a.Bin
	}
	id, _, found := strings.Cut(a.ID, "@wsl:")
	if !found {
		return a.ID
	}
	if local, err := FindIn(all, id); err == nil {
		if metadata := local.extensionMetadata; metadata != nil && metadata.base != nil {
			local = metadata.base
		}
		if local.Bin != "" {
			return local.Bin
		}
	}
	if a.Plugin != "" {
		plugins, _ := agentplug.List()
		for _, plugin := range plugins {
			if plugin.Spec == a.Plugin && plugin.Desc.ID == id && plugin.Desc.Bin != "" {
				return plugin.Desc.Bin
			}
		}
	}
	return id
}

type extensionAgentMetadata struct {
	plugin                string
	descriptor            sdk.AgentDescriptor
	base                  *Agent
	inheritedLaunchConfig string
	owns                  func() bool
	disconnected          func() bool
}

func (a *Agent) SettingsPath() string {
	if a.Path != "" {
		return a.Path
	}
	if metadata := a.extensionMetadata; metadata != nil && metadata.inheritedLaunchConfig != "" &&
		metadata.owns != nil && metadata.owns() {
		return metadata.inheritedLaunchConfig
	}
	return ""
}

func extensionProfileCatalog(ctx context.Context, caller, target string) string {
	if caller == "" || caller == target {
		return target
	}
	ledger, err := extensionBindingRead()
	if err != nil {
		return target
	}
	home, _ := homes()
	for _, claim := range ledger.Records {
		if claim.Agent != caller || claim.Phase != "owned" || claim.Descriptor.Profile == nil ||
			claim.Descriptor.Profile.Target != target || !extensionSameHome(claim.Home, home) {
			continue
		}
		host, err := inspectAgentRuntime(ctx, extension.Default(), claim.Plugin, claim.Descriptor)
		if err == nil && host.entry.Manifest.Contract.Has(sdk.AgentWiring) &&
			host.entry.Revision == claim.PackageRevision && host.config == claim.ConfigVersion {
			return caller
		}
	}
	return target
}

func (a *Agent) HasSDKAgentDeclaration() bool {
	return a.extensionMetadata != nil
}

func (a *Agent) SDKLibraryTarget() (*Agent, *sdk.AgentLibraryTarget, bool) {
	metadata := a.extensionMetadata
	if metadata == nil {
		if a.coreLibrary != nil {
			library := *a.coreLibrary
			return a, &library, true
		}
		return nil, nil, false
	}
	if metadata.descriptor.Profile != nil && (metadata.owns == nil || !metadata.owns()) {
		return nil, nil, true
	}
	target := a
	if metadata.base != nil {
		target = metadata.base
	}
	if metadata.descriptor.Library != nil {
		home, _ := homes()
		library := metadata.descriptor.Library.Resolve(home)
		return target, &library, true
	}
	if metadata.base != nil {
		return metadata.base, nil, true
	}
	return nil, nil, true
}
