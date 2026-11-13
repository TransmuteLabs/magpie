package agent

import (
	"strings"

	"github.com/yetone/magpie/internal/agentplug"
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
	plugin       string
	descriptor   sdk.AgentDescriptor
	base         *Agent
	owns         func() bool
	disconnected func() bool
}

func (a *Agent) SDKLibraryTarget() (*Agent, *sdk.AgentLibrary, bool) {
	metadata := a.extensionMetadata
	if metadata == nil {
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
		library := *metadata.descriptor.Library
		return target, &library, true
	}
	if metadata.base != nil {
		return metadata.base, nil, true
	}
	return nil, nil, true
}
