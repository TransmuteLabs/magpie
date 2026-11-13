package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/windowmeta"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func validateWindowPackage(ctx context.Context, root *os.Root, manifest sdk.Manifest) error {
	if manifest.Window == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := catalogReadFile(root, manifest.Window.Metadata, catalogFileLimit)
	if err != nil {
		return fmt.Errorf("module window metadata: %w", err)
	}
	var object map[string]json.RawMessage
	if err := catalogDecode(data, &object); err != nil || object == nil {
		return errors.New("module window metadata must be an unambiguous JSON object")
	}
	directory := filepath.Join(root.Name(), filepath.FromSlash(path.Dir(manifest.Window.Metadata)))
	window, _ := windowmeta.WindowFromData(directory, data)
	if window == nil || window.Page == nil && len(window.Panels) == 0 {
		return errors.New("module declares a window but its metadata provides no valid page or panel")
	}
	for _, binding := range manifest.Bindings {
		if binding.From == "window:page" {
			if window.Page == nil {
				return fmt.Errorf("module binding %q requires a declared window page", binding.ID)
			}
		} else if index, panel := strings.CutPrefix(binding.From, "window:panel:"); panel {
			number, err := strconv.Atoi(index)
			if err != nil || strconv.Itoa(number) != index || number < 0 || number >= len(window.Panels) {
				return fmt.Errorf("module binding %q requires an existing window panel", binding.ID)
			}
		}
	}
	return ctx.Err()
}
