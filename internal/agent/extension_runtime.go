package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionAgentRuntime struct {
	manager *extension.Manager
	lease   *extension.Lease
	entry   extension.Installed
	config  string
}

func (host *extensionAgentRuntime) Manifest(plugin string) (sdk.Manifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := host.check(ctx, plugin); err != nil {
		return sdk.Manifest{}, err
	}
	return host.entry.Manifest.Clone(), nil
}

func (host *extensionAgentRuntime) check(ctx context.Context, plugin string) error {
	entries, err := host.manager.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != plugin || entry.Disabled {
			continue
		}
		config, err := host.manager.ModuleConfig(ctx, plugin)
		if err != nil {
			return err
		}
		if entry.Revision != host.entry.Revision || config.Version != host.config {
			return extension.ErrActionChanged
		}
		return nil
	}
	return errors.New("agent module is disabled or unavailable")
}

func (host *extensionAgentRuntime) Store() *extension.Store { return host.manager.Store() }

func (host *extensionAgentRuntime) Invoke(ctx context.Context, plugin, method string, scope sdk.Scope, params, out any, consume func(extension.Event) error) error {
	if err := host.check(ctx, plugin); err != nil {
		return err
	}
	raw, err := host.lease.Call(ctx, method, scope, params, consume)
	if err == nil && out != nil {
		err = json.Unmarshal(raw, out)
	}
	return err
}

func inspectAgentRuntime(ctx context.Context, manager *extension.Manager, plugin string, descriptor sdk.AgentDescriptor) (*extensionAgentRuntime, error) {
	entries, err := manager.List()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != plugin || entry.Disabled {
			continue
		}
		found := false
		for _, actual := range entry.Manifest.Agents {
			if actual.ID == descriptor.ID && extensionBindingSameDescriptor(actual, descriptor) {
				found = true
			}
		}
		if !found {
			return nil, errors.New("agent module definition changed; rediscover the agent")
		}
		config, err := manager.ModuleConfig(ctx, plugin)
		if err != nil {
			return nil, err
		}
		return &extensionAgentRuntime{manager: manager, entry: entry, config: config.Version}, nil
	}
	return nil, errors.New("agent module is disabled or unavailable")
}

func acquireAgentRuntime(ctx context.Context, manager *extension.Manager, plugin string, descriptor sdk.AgentDescriptor) (*extensionAgentRuntime, error) {
	host, err := inspectAgentRuntime(ctx, manager, plugin, descriptor)
	if err != nil {
		return nil, err
	}
	host.lease, err = manager.AcquireRevision(ctx, plugin, host.entry.Revision, host.config)
	if err != nil {
		return nil, err
	}
	return host, nil
}
