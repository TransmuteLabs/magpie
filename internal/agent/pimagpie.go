package agent

import (
	"encoding/json"
	"os"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/lazyre"
)

// piMagpieExt matches a Pi package source for the pi-magpie extension:
// npm:pi-magpie, npm:pi-magpie@0.1.6, or a path or git source ending in it.
var piMagpieExt = lazyre.New(`(^|[:/\\])pi-magpie(@[^/\\]*)?(#.*)?$`)

// piMagpieInstalled says Pi's settings.json lists the pi-magpie extension
// in "packages". It serves provider "magpie" from magpie's live catalog,
// and warns at every start while models.json also names providers.magpie:
// that block gives every model one baseUrl and a frozen list. A settings
// file that can't be read is unknown, so not installed.
func piMagpieInstalled(settings string) bool {
	raw, err := os.ReadFile(settings)
	return err == nil && piMagpieSettings(raw)
}

func piMagpieSettings(raw []byte) bool {
	var s struct {
		Packages []json.RawMessage `json:"packages"`
	}
	if json.Unmarshal(edit.TrimBOM(raw), &s) != nil {
		return false
	}
	for _, p := range s.Packages {
		var src string
		if json.Unmarshal(p, &src) != nil {
			var o struct {
				Source string `json:"source"`
			}
			if json.Unmarshal(p, &o) != nil {
				continue
			}
			src = o.Source
		}
		if piMagpieExt.MatchString(src) {
			return true
		}
	}
	return false
}
