package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/yetone/magpie/internal/windowmeta"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type PluginPresentation struct {
	Description string
	Name        string
	Icon        string
	Window      *windowmeta.WindowSpec
}

func (m *Manager) PluginDescription(ctx context.Context, entry Installed) string {
	return m.PluginPresentation(ctx, entry).Description
}

func (m *Manager) PluginPresentation(ctx context.Context, entry Installed) PluginPresentation {
	const filename = "package.json"
	if _, present := entry.Files[filename]; (!present && entry.Manifest.Window == nil) || ctx.Err() != nil {
		return PluginPresentation{}
	}
	root, err := m.catalog.open(false)
	if err != nil {
		return PluginPresentation{}
	}
	defer root.Close()
	directory, err := openWindowDirectory(root, entry.Directory)
	if err != nil {
		return PluginPresentation{}
	}
	defer directory.Close()
	body := presentationFile(directory, entry, filename)
	out := decodePluginPresentation(body)
	if declaration := entry.Manifest.Window; declaration != nil && ctx.Err() == nil {
		if declaration.Metadata != filename {
			body = presentationFile(directory, entry, declaration.Metadata)
			ui := decodePluginPresentation(body)
			if out.Name == "" {
				out.Name = ui.Name
			}
			if out.Icon == "" {
				out.Icon = ui.Icon
			}
		}
		target := filepath.Join(directory.Name(), filepath.FromSlash(path.Dir(declaration.Metadata)))
		out.Window, _ = windowmeta.WindowFromData(target, body)
		if out.Name == "" && out.Window != nil && out.Window.Page != nil {
			out.Name = windowmeta.PluginDisplayName(out.Window.Page.Title)
		}
	}
	return out
}

func presentationFile(directory *os.Root, entry Installed, filename string) []byte {
	expected, present := entry.Files[filename]
	if !present {
		return nil
	}
	parent, err := openWindowDirectory(directory, path.Dir(filename))
	if err != nil {
		return nil
	}
	defer parent.Close()
	body, err := catalogReadFile(parent, path.Base(filename), releaseMetadataLimit)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != expected {
		return nil
	}
	return body
}

func decodePluginPresentation(body []byte) (out PluginPresentation) {
	var metadata struct {
		Description string          `json:"description"`
		Magpie      json.RawMessage `json:"magpie"`
	}
	if json.Unmarshal(body, &metadata) != nil {
		return out
	}
	description := strings.TrimSpace(metadata.Description)
	if !strings.ContainsFunc(description, unicode.IsControl) {
		out.Description = description
	}
	var brand struct {
		Name string `json:"name"`
		Icon string `json:"icon"`
	}
	if json.Unmarshal(metadata.Magpie, &brand) == nil {
		out.Name = windowmeta.PluginDisplayName(brand.Name)
		if icon := strings.TrimSpace(brand.Icon); sdk.ValidID(icon) {
			out.Icon = icon
		}
	}
	return out
}
