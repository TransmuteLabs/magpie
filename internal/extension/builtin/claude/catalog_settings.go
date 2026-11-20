package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/edit"
)

const (
	CapabilitiesEnv = "CLAUDE_CODE_MODEL_CAPABILITIES"
	CustomOptionEnv = "ANTHROPIC_CUSTOM_MODEL_OPTION"
)

type ModelPicker struct {
	Options               []ModelPickerRow `json:"options"`
	ReplaceBuiltInOptions bool             `json:"replaceBuiltInOptions,omitempty"`
}

type ModelPickerRow struct {
	Description string `json:"description"`
	Label       string `json:"label"`
	Model       string `json:"model"`
}

type CatalogClaims struct {
	Picker, Capabilities string
}

type CatalogOptions struct {
	Previous     CatalogClaims
	ManagePicker bool
	Picker       func(customOption bool) ModelPicker
	Capabilities func() string
}

func pickerFingerprint(raw string) string {
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(raw)) != nil {
		return ""
	}
	return hashPicker(compact.Bytes())
}

func hashPicker(compact []byte) string {
	sum := sha256.Sum256(compact)
	return hex.EncodeToString(sum[:])
}

func PickerOwned(raw, claim string) bool {
	if claim != "" && claim != "1" {
		return pickerFingerprint(raw) == claim
	}
	var picker ModelPicker
	if json.Unmarshal([]byte(raw), &picker) != nil || len(picker.Options) == 0 {
		return false
	}
	for _, row := range picker.Options {
		if row.Model == "" || !strings.HasSuffix(row.Description, "via magpie") {
			return false
		}
	}
	return true
}

func catalogChanges(current gjson.Result, options *CatalogOptions) ([]edit.KV, []string, CatalogClaims, error) {
	if options == nil {
		return nil, nil, CatalogClaims{}, nil
	}
	var set []edit.KV
	var remove []string
	claims := options.Previous
	if options.ManagePicker {
		written := current.Get("modelPicker")
		if !written.Exists() || PickerOwned(written.Raw, claims.Picker) {
			previous := claims.Picker
			claims.Picker = ""
			var picker ModelPicker
			if options.Picker != nil {
				picker = options.Picker(current.Get("env."+CustomOptionEnv).String() != "")
			}
			if len(picker.Options) > 0 {
				body, err := json.Marshal(picker)
				if err != nil {
					return nil, nil, CatalogClaims{}, err
				}
				if previous == "" || previous == "1" {
					previous = pickerFingerprint(written.Raw)
				}
				claims.Picker = hashPicker(body)
				if !written.Exists() || claims.Picker != previous {
					set = append(set, edit.KV{Path: "modelPicker", Value: json.RawMessage(body)})
				}
			} else if written.Exists() {
				remove = append(remove, "modelPicker")
			}
		}
	}
	const capsPath = "env." + CapabilitiesEnv
	caps := current.Get(capsPath).String()
	claims.Capabilities = ""
	if caps == "" || caps == options.Previous.Capabilities {
		if options.Capabilities != nil {
			claims.Capabilities = options.Capabilities()
		}
		if claims.Capabilities != caps {
			if claims.Capabilities == "" {
				remove = append(remove, capsPath)
			} else {
				set = append(set, edit.KV{Path: capsPath, Value: claims.Capabilities})
			}
		}
	}
	return set, remove, claims, nil
}
