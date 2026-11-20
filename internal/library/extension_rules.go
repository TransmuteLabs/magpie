package library

import (
	"context"

	"github.com/yetone/magpie/internal/extension"
)

func ImportExtensionRule(ctx context.Context, source, revision, name string, agents []string) (*Result, error) {
	manager := extension.Default()
	return change(func(l *Library) error {
		selection, err := manager.LibraryRule(ctx, source, revision)
		if err != nil {
			return err
		}
		rule := Rule{Name: name, Body: selection.Body, Paths: selection.Paths, Agents: agents}
		if err := prepareRule(&rule); err != nil {
			return err
		}
		return l.putRule("", rule)
	})
}
