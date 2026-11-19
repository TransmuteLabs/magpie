package plugininstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func SDKSource(ctx context.Context, spec string) (string, bool, error) {
	spec = strings.TrimSpace(spec)
	if source, explicit := strings.CutPrefix(spec, "sdk:"); explicit {
		if plugin.IsPath(source) {
			source = strings.TrimPrefix(source, "file://")
		}
		return source, true, nil
	}
	if plugin.IsPath(spec) {
		source := strings.TrimPrefix(spec, "file://")
		info, err := os.Stat(source)
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if !info.IsDir() {
			return "", false, nil
		}
		_, err = os.Lstat(filepath.Join(source, sdk.ManifestFile))
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return source, true, err
	}
	if strings.HasPrefix(spec, "github:") && strings.Contains(spec, "#") {
		for _, entry := range plugin.Load().Plugins {
			if entry.Spec == spec {
				return "", false, nil
			}
		}
		handled, err := extension.Default().ProbePluginRelease(ctx, spec)
		return spec, handled, err
	}
	return "", false, nil
}

func InstalledSDK(spec string) (extension.Installed, bool, error) {
	id, explicit := strings.CutPrefix(spec, "sdk:")
	if !explicit {
		for _, entry := range plugin.Load().Plugins {
			name := plugin.Name(entry.Spec)
			if spec == entry.Spec || spec == name || plugin.Called(name, spec) {
				return extension.Installed{}, false, nil
			}
		}
	}
	entries, err := extension.Default().List()
	if err != nil {
		return extension.Installed{}, false, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id || !explicit && entry.Source != nil && entry.Source.Reference == spec {
			return entry, true, nil
		}
	}
	if explicit {
		return extension.Installed{}, false, fmt.Errorf("unknown extension %q", id)
	}
	return extension.Installed{}, false, nil
}
