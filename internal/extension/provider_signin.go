package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type ProviderSignIn struct {
	Module   string
	Provider string
	Name     string
	Page     bool
	Revision string
	Binding  sdk.WindowAuthBindingDescriptor
}

func (m *Manager) ProviderSignIns(ctx context.Context) []ProviderSignIn {
	entries, err := m.ListContext(ctx)
	if err != nil {
		return nil
	}
	var out []ProviderSignIn
	for _, entry := range entries {
		manifest := entry.Manifest
		if entry.Disabled || manifest.Component != nil || entry.observerEnablementErr != nil ||
			len(manifest.AuthBindings) == 0 || sdk.CheckCompatibility(manifest) != nil {
			continue
		}
		var revision string
		if manifest.Contract.Has(sdk.ProviderSignIn) {
			active, config, err := m.ActiveModuleConfig(ctx, manifest.ID)
			if err != nil || active.Revision != entry.Revision {
				continue
			}
			revision = providerSignInRevision(active, config.Version)
		}
		for _, descriptor := range manifest.Providers {
			target := ProviderSignIn{Module: manifest.ID, Provider: descriptor.ID, Name: descriptor.Name}
			index := slices.IndexFunc(manifest.AuthBindings, func(binding sdk.WindowAuthBindingDescriptor) bool {
				return binding.Provider == descriptor.ID && binding.From == sdk.ProviderSignInPart
			})
			if index >= 0 {
				if revision != "" {
					target.Binding, _ = manifest.ResolveAuthBinding(manifest.AuthBindings[index].ID)
					target.Revision = revision
					out = append(out, target)
				}
				continue
			}
			if slices.ContainsFunc(manifest.AuthBindings, func(binding sdk.WindowAuthBindingDescriptor) bool {
				return binding.Provider == descriptor.ID && binding.From == "window:page"
			}) {
				target.Page = true
				out = append(out, target)
			}
		}
	}
	return out
}

func providerSignInRevision(entry Installed, config string) string {
	return storeHash(sdk.ProviderSignInPart, entry.Revision, config, entry.EnableGeneration)
}

func (m *Manager) authEntry(ctx context.Context, id, revision, part string) (Installed, sdk.ConfigSnapshot, error) {
	if part != sdk.ProviderSignInPart {
		return m.windowEntry(ctx, id, revision)
	}
	entry, config, err := m.ActiveModuleConfig(ctx, id)
	if err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	if err := sdk.CheckCompatibility(entry.Manifest); err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	if revision != providerSignInRevision(entry, config.Version) {
		return Installed{}, sdk.ConfigSnapshot{}, errors.New("provider sign-in generation changed")
	}
	return entry, config, ctx.Err()
}

func windowAuthJournalSelection(entry Installed, config, part string) (string, string) {
	if part == sdk.ProviderSignInPart {
		return "window:page", providerSignInRevision(entry, config)
	}
	return part, entry.Revision
}

func providerSignInInput(method sdk.WindowAuthMethod, input json.RawMessage) bool {
	input = bytes.TrimSpace(input)
	if len(input) == 0 || bytes.Equal(input, []byte("null")) {
		input = json.RawMessage("{}")
	}
	var fields map[string]json.RawMessage
	if catalogDecode(input, &fields) != nil || fields == nil || len(fields) > len(method.Fields) {
		return false
	}
	for _, field := range method.Fields {
		raw, found := fields[field.Key]
		if !found {
			if field.Optional {
				continue
			}
			return false
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || len(value) == 0 || len(value) > 16*1024 ||
			field.Type == "select" && !slices.ContainsFunc(field.Choices, func(choice sdk.AuthInputChoice) bool { return choice.Value == value }) {
			return false
		}
		delete(fields, field.Key)
	}
	return len(fields) == 0
}
