package library

import (
	"context"

	"github.com/yetone/magpie/internal/extension"
)

func ImportExtensionRule(ctx context.Context, source, revision, name string, agents []string) (*Result, error) {
	manager := extension.Default()
	return changeExtensionLibrary(ctx, func(l *Library) (extension.LibraryPublication, error) {
		selection, publication, err := manager.LibraryRule(ctx, source, revision)
		if err != nil {
			return extension.LibraryPublication{}, err
		}
		rule := Rule{Name: name, Body: selection.Body, Paths: selection.Paths, Agents: agents}
		if err := prepareRule(&rule); err != nil {
			return extension.LibraryPublication{}, err
		}
		return publication, l.putRule("", rule)
	})
}

func changeExtensionLibrary(ctx context.Context, apply func(*Library) (extension.LibraryPublication, error)) (*Result, error) {
	var publication extension.LibraryPublication
	return changeWithPublication(func(l *Library) error {
		var err error
		publication, err = apply(l)
		return err
	}, func() (func(), error) {
		return publication.Lock(ctx)
	})
}
