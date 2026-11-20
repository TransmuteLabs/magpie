package extension

import "context"

func (m *Manager) ActiveModules(ctx context.Context) ([]Installed, error) {
	return m.readContributionEntries(ctx)
}
