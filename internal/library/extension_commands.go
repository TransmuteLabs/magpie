package library

import (
	"context"

	"github.com/yetone/magpie/internal/extension"
)

func ImportExtensionCommand(ctx context.Context, source, revision, name string, agents []string) (*Result, error) {
	manager := extension.Default()
	return changeExtensionLibrary(ctx, func(l *Library) (extension.LibraryPublication, error) {
		selection, publication, err := manager.LibraryCommand(ctx, source, revision)
		if err != nil {
			return extension.LibraryPublication{}, err
		}
		command := Command{Name: name, Description: selection.Description, Prompt: selection.Prompt, Agents: agents}
		if err := prepareCommand(&command); err != nil {
			return extension.LibraryPublication{}, err
		}
		return publication, l.putCommand("", command)
	})
}
