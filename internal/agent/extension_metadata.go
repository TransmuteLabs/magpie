package agent

import sdk "github.com/yetone/magpie/pkg/extension"

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
