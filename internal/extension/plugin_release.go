package extension

import (
	"context"
	"errors"
	"strings"

	"github.com/yetone/magpie/internal/netproxy"
	"golang.org/x/mod/semver"
)

type ReleaseAvailability struct {
	Version   string `json:"version,omitempty"`
	Available bool   `json:"available"`
}

func (m *Manager) ProbePluginRelease(ctx context.Context, source string) (bool, error) {
	module := releaseModule(source)
	if module == "" {
		return false, nil
	}
	_, pin, err := releaseSource(source)
	if err != nil {
		return false, nil
	}
	if pin != "" {
		return true, nil
	}
	release, err := m.pluginRelease(ctx, source)
	if errors.Is(err, errNoModuleRelease) || errors.Is(err, errReleaseCredentials) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, asset := range release.Assets {
		if strings.HasPrefix(asset.Name, releaseArchivePrefix(module)) && strings.HasSuffix(asset.Name, ".zip") {
			_, _, _, err := releaseSelect(release.Assets, module)
			return true, err
		}
	}
	return false, nil
}

func (m *Manager) CheckUpdate(ctx context.Context, entry Installed) (ReleaseAvailability, error) {
	current := entry.Manifest.Version
	if entry.Source == nil || strings.Contains(entry.Source.Reference, "@") {
		return ReleaseAvailability{Version: current}, nil
	}
	reference := entry.Source.Reference
	release, err := m.pluginRelease(ctx, reference)
	if err != nil {
		return ReleaseAvailability{}, err
	}
	module := releaseModule(reference)
	version := strings.TrimPrefix(release.Tag, "v")
	if module != "" {
		version, _ = releaseModuleVersion(release.Tag, module)
	}
	if semver.Compare("v"+version, "v"+current) <= 0 {
		return ReleaseAvailability{Version: current}, nil
	}
	if _, _, _, err := releaseSelect(release.Assets, module); err != nil {
		return ReleaseAvailability{}, err
	}
	return ReleaseAvailability{Version: version, Available: true}, nil
}

func (m *Manager) pluginRelease(ctx context.Context, reference string) (githubRelease, error) {
	client := m.ReleaseClient
	if client == nil {
		client = releaseHTTPClient()
		defer client.CloseIdleConnections()
	}
	release, _, err := resolveRelease(netproxy.Downloads(ctx), client, reference, defaultReleaseToken)
	return release, err
}
