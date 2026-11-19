package gui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/plugininstall"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sdkPluginJSON struct {
	Description  string               `json:"description,omitempty"`
	Source       string               `json:"source,omitempty"`
	Runtime      string               `json:"runtime"`
	API          string               `json:"api"`
	HostAPI      string               `json:"hostAPI,omitempty"`
	NativeABI    uint32               `json:"nativeABI,omitempty"`
	Platforms    []string             `json:"platforms,omitempty"`
	Capabilities []sdk.Capability     `json:"capabilities"`
	Agents       []string             `json:"agents,omitempty"`
	Inbounds     []string             `json:"inbounds,omitempty"`
	Protocols    []string             `json:"protocols,omitempty"`
	Sessions     []string             `json:"sessions,omitempty"`
	Window       bool                 `json:"window,omitempty"`
	Checkable    bool                 `json:"checkable,omitempty"`
	Check        *plugin.VersionCheck `json:"check,omitempty"`
}

type sdkPluginCheck struct {
	revision string
	source   extension.PackageSource
	check    plugin.VersionCheck
}

var sdkPluginChecks struct {
	sync.Mutex
	root string
	byID map[string]sdkPluginCheck
}

func sdkPluginRow(ctx context.Context, host *extension.Manager, entry extension.Installed) pluginEntryJSON {
	manifest := entry.Manifest
	version := manifest.Contract.Version
	info := &sdkPluginJSON{
		Description: host.PluginDescription(ctx, entry),
		Runtime:     manifest.Runtime, API: fmt.Sprintf("%d.%d", version.Major, version.Minor),
		Capabilities: manifest.Contract.Capabilities, Window: manifest.Window != nil,
		Checkable: sdkPluginCheckable(entry),
	}
	row := pluginEntryJSON{
		Entry:   plugin.Entry{Spec: "sdk:" + manifest.ID, Off: entry.Disabled},
		OwnName: manifest.ID, Version: manifest.Version, SDK: info,
		Providers: []string{}, Moved: []string{},
		IsAgent: len(manifest.Agents) != 0, InMagpieOnly: len(manifest.Providers) == 0,
		IsMiddleware: manifest.Middleware != nil, IsGateway: manifest.Hooks != nil,
	}
	if err := sdk.CheckCompatibility(manifest); err != nil {
		row.Error = err.Error()
	}
	if entry.Source != nil {
		info.Source = entry.Source.Reference
	}
	if compatibility := manifest.Compatibility; compatibility != nil {
		info.HostAPI, info.NativeABI, info.Platforms = compatibility.HostAPI, compatibility.NativeABI, compatibility.Platforms
	}
	for _, provider := range manifest.Providers {
		row.Providers = append(row.Providers, provider.Name)
		for _, protocol := range provider.Protocols {
			if !slices.Contains(info.Protocols, string(protocol)) {
				info.Protocols = append(info.Protocols, string(protocol))
			}
		}
	}
	for _, agent := range manifest.Agents {
		info.Agents = append(info.Agents, agent.Name)
	}
	for _, inbound := range manifest.Inbounds {
		info.Inbounds = append(info.Inbounds, inbound.Path)
	}
	for _, session := range manifest.Sessions {
		info.Sessions = append(info.Sessions, session.Name)
	}
	for _, source := range manifest.SessionSources {
		info.Sessions = append(info.Sessions, source.ID)
	}
	for _, source := range manifest.StorageSources {
		info.Sessions = append(info.Sessions, source.ID)
	}
	if check, ok := cachedSDKPluginCheck(host, entry); ok {
		info.Check = &check
		row.Latest = check.Latest
	}
	return row
}

func sdkPluginCheckable(entry extension.Installed) bool {
	return entry.Source != nil && !strings.Contains(entry.Source.Reference, "@")
}

func cachedSDKPluginCheck(host *extension.Manager, entry extension.Installed) (plugin.VersionCheck, bool) {
	if !sdkPluginCheckable(entry) {
		return plugin.VersionCheck{}, false
	}
	sdkPluginChecks.Lock()
	defer sdkPluginChecks.Unlock()
	saved, ok := sdkPluginChecks.byID[entry.Manifest.ID]
	return saved.check, ok && sdkPluginChecks.root == host.Namespace() && matchesSDKPluginCheck(saved, entry)
}

func matchesSDKPluginCheck(saved sdkPluginCheck, entry extension.Installed) bool {
	return entry.Source != nil && saved.revision == entry.Revision && saved.source == *entry.Source
}

func checkSDKPlugins(ctx context.Context) ([]plugin.VersionCheck, error) {
	host := extension.Default()
	entries, err := host.List()
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(entries, func(entry extension.Installed) bool { return !sdkPluginCheckable(entry) })
	checks := make([]plugin.VersionCheck, len(entries))
	var workers sync.WaitGroup
	queue := make(chan int)
	for range min(len(entries), 4) {
		workers.Go(func() {
			for index := range queue {
				entry := entries[index]
				id := entry.Manifest.ID
				check := plugin.VersionCheck{Spec: "sdk:" + id, Package: id, Version: entry.Manifest.Version, Status: "current", Off: entry.Disabled}
				latest, err := host.CheckUpdate(ctx, entry)
				if err != nil {
					check.Status, check.Error = "unknown", err.Error()
				} else if latest.Available {
					check.Status, check.Latest = "update", latest.Version
				}
				checks[index] = check
			}
		})
	}
	for index := range entries {
		queue <- index
	}
	close(queue)
	workers.Wait()
	byID := make(map[string]sdkPluginCheck, len(entries))
	for index, entry := range entries {
		byID[entry.Manifest.ID] = sdkPluginCheck{revision: entry.Revision, source: *entry.Source, check: checks[index]}
	}
	current, err := host.List()
	if err != nil {
		return nil, err
	}
	checks = checks[:0]
	for _, entry := range current {
		if saved, found := byID[entry.Manifest.ID]; found && matchesSDKPluginCheck(saved, entry) {
			saved.check.Off = entry.Disabled
			checks = append(checks, saved.check)
		}
	}
	sdkPluginChecks.Lock()
	sdkPluginChecks.root = host.Namespace()
	sdkPluginChecks.byID = byID
	sdkPluginChecks.Unlock()
	return checks, nil
}

func pendingPluginUpdates() plugin.Updates {
	updates := plugin.PendingUpdates()
	host := extension.Default()
	entries, err := host.List()
	if err != nil {
		return updates
	}
	for _, entry := range entries {
		if check, ok := cachedSDKPluginCheck(host, entry); !entry.Disabled && ok && check.Status == "update" {
			updates.Waiting = append(updates.Waiting, plugin.Waiting{
				Spec: check.Spec, Package: check.Package, Version: check.Version, Latest: check.Latest,
			})
		}
	}
	return updates
}

func sdkPluginOperation(ctx context.Context, op, spec string, off bool) (bool, error) {
	switch op {
	case "add":
		source, handled, err := plugininstall.SDKSource(ctx, spec)
		if err == nil && handled {
			_, err = extension.Default().Install(ctx, source)
		}
		return handled, err
	case "off", "remove", "upgrade":
		entry, handled, err := plugininstall.InstalledSDK(spec)
		if err != nil || !handled {
			return handled, err
		}
		host := extension.Default()
		id := entry.Manifest.ID
		switch op {
		case "off":
			err = host.SetEnabled(ctx, id, !off)
		case "remove":
			err = host.Remove(ctx, id)
		case "upgrade":
			_, err = host.Update(ctx, id)
		}
		return true, err
	}
	return false, nil
}

func updateSDKPlugins(ctx context.Context) error {
	host := extension.Default()
	entries, err := host.List()
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if !sdkPluginCheckable(entry) {
			continue
		}
		if _, err := host.Update(ctx, entry.Manifest.ID); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", entry.Manifest.ID, err))
		}
	}
	return errors.Join(failures...)
}
