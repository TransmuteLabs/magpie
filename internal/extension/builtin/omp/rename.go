package omp

import (
	"errors"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
	"gopkg.in/yaml.v3"
)

type RenameOptions struct {
	From, To  string
	Originals map[string]string
}

func PlanRename(options RenameOptions, settings sdk.ActionSettings) (sdk.AgentPlan, bool, map[string]string, error) {
	var plan sdk.AgentPlan
	if len(settings.Files) != 1 || settings.Files[0].Path != ConfigFile {
		return plan, false, nil, errors.New("OMP rename action requires a config.yml snapshot")
	}
	old, replacement := "magpie/"+options.From+"/", "magpie/"+options.To+"/"
	move := func(value string) string {
		return MapReferences(value, func(model string) string {
			if rest, ok := strings.CutPrefix(model, old); ok {
				return replacement + rest
			}
			return model
		})
	}
	file := settings.Files[0]
	moved := false
	body, err := edit.RewriteYAMLStrings(file.Path, file.Body, ModelReferenceKeys, func(value string) string {
		next := move(value)
		moved = moved || next != value
		return next
	})
	if err != nil {
		return plan, false, nil, err
	}
	body, err = edit.RewriteYAMLNode(file.Path, body, "enabledModels", uniqueScope)
	if err != nil {
		return plan, false, nil, err
	}
	updated := map[string]string{}
	for key, original := range options.Originals {
		next := move(original)
		if YAMLList(original) {
			text, err := edit.EditYAMLTextStrings(edit.YAMLText(original), move)
			if err != nil {
				return plan, false, nil, err
			}
			next = string(text)
		}
		if next != original {
			updated[key] = next
		}
	}
	appendChange(&plan, file, body)
	return plan, moved, updated, nil
}

func uniqueScope(node *yaml.Node) (bool, bool) {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false, false
	}
	seen := map[string]bool{}
	kept := node.Content[:0:0]
	for _, item := range node.Content {
		if item.Kind == yaml.ScalarNode {
			if seen[item.Value] {
				continue
			}
			seen[item.Value] = true
		}
		kept = append(kept, item)
	}
	if len(kept) == len(node.Content) {
		return false, false
	}
	node.Content = kept
	return true, false
}
