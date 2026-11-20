package extension

import (
	"context"
	"fmt"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type LibraryCommand struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	sdk.LibraryCommandDescriptor
}

func (m *Manager) LibraryCommands(ctx context.Context) ([]LibraryCommand, error) {
	entries, err := m.ActiveModules(ctx)
	if err != nil {
		return nil, err
	}
	commands := []LibraryCommand{}
	for _, entry := range entries {
		for _, contribution := range entry.Manifest.Contributions {
			if contribution.Point == sdk.ContributionLibraryCommand && contribution.Version == 1 {
				commands = append(commands, m.libraryCommand(entry, *contribution.LibraryCommand))
			}
		}
	}
	return commands, nil
}

func (m *Manager) LibraryCommand(ctx context.Context, name, expected string) (LibraryCommand, LibraryPublication, error) {
	module, id, valid := contributionName(name)
	if !valid || !catalogDigest(expected) {
		return LibraryCommand{}, LibraryPublication{}, fmt.Errorf("select a Library command by module/id and its listed revision")
	}
	entry, err := m.ActiveModule(ctx, module)
	if err != nil {
		return LibraryCommand{}, LibraryPublication{}, err
	}
	for _, contribution := range entry.Manifest.Contributions {
		if contribution.Point == sdk.ContributionLibraryCommand && contribution.Version == 1 && contribution.LibraryCommand.ID == id {
			command := m.libraryCommand(entry, *contribution.LibraryCommand)
			if command.Revision == expected {
				return command, m.libraryPublication(entry), nil
			}
			break
		}
	}
	return LibraryCommand{}, LibraryPublication{}, errContributionRevoked
}

func (m *Manager) libraryCommand(entry Installed, descriptor sdk.LibraryCommandDescriptor) LibraryCommand {
	name := entry.Manifest.ID + "/" + descriptor.ID
	return LibraryCommand{
		Name: name, Revision: storeHash(m.Namespace(), string(sdk.ContributionLibraryCommand), name, entry.Revision, entry.EnableGeneration),
		LibraryCommandDescriptor: descriptor,
	}
}
