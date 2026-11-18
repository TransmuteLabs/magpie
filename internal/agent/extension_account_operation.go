package agent

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	gateway.RegisterProtocolAgentBindingResolver(protocolAgentBinding)
}

func protocolAgentBinding(ctx context.Context, lease *extension.Lease, id string) (gateway.ProtocolAgentBinding, error) {
	manifest := lease.Manifest()
	position := slices.IndexFunc(manifest.Agents, func(descriptor sdk.AgentDescriptor) bool { return descriptor.ID == id })
	if position < 0 {
		return gateway.ProtocolAgentBinding{}, errors.New("native account operation agent was not declared")
	}
	home, _ := homes()
	descriptor := manifest.Agents[position]
	controller := &extensionController{home: home, item: ExtensionAgent{Plugin: manifest.ID, Agent: descriptor},
		bin: descriptor.Bin, root: extensionBindingPath(), rootGuard: extensionAgentRootGuard(home)}
	record, err := controller.read(ctx)
	if err != nil {
		return gateway.ProtocolAgentBinding{}, err
	}
	valid := func(record extensionBindingRecord) bool {
		return record.Phase == "owned" && record.Revision != 0 && record.Values["model"] != "" &&
			record.PackageRevision == lease.Revision() && record.ConfigVersion == lease.ConfigVersion() &&
			extensionBindingSameDescriptor(record.Descriptor, descriptor)
	}
	if !valid(record) {
		return gateway.ProtocolAgentBinding{}, errors.New("native account operation agent ownership changed")
	}
	return gateway.ProtocolAgentBinding{Model: record.Values["model"], Revision: strconv.FormatUint(record.Revision, 10),
		Check: func(ctx context.Context) error {
			current, err := controller.read(ctx)
			if err != nil || !valid(current) || current.Revision != record.Revision ||
				current.Values["model"] != record.Values["model"] {
				return errors.New("native account operation agent binding changed")
			}
			return nil
		}}, nil
}
