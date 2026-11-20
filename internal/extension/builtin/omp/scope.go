package omp

import (
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
)

type ScopeOptions struct {
	Emptied bool
	Next    func(plain []string, present bool, offered map[string]bool) (next []string, changed bool)
}

func PlainScope(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var plain []string
	for _, item := range node.Content {
		if item.Kind == yaml.ScalarNode && item.ShortTag() == "!!str" {
			plain = append(plain, item.Value)
		}
	}
	return plain
}

func rewriteScope(options ScopeOptions, config, catalog []byte) ([]byte, bool, error) {
	var doc struct {
		Providers map[string]struct {
			Models []struct {
				ID string `yaml:"id"`
			} `yaml:"models"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal(catalog, &doc) != nil {
		return config, false, nil
	}
	offered := map[string]bool{}
	for _, model := range doc.Providers["magpie"].Models {
		if id := strings.TrimSpace(model.ID); id != "" {
			offered[strings.ToLower("magpie/"+id)] = true
		}
	}
	if len(offered) == 0 {
		return config, false, nil
	}
	const key = "enabledModels"
	synced := true
	var made []string
	body, err := edit.RewriteYAMLNode(ConfigFile, config, key, func(node *yaml.Node) (bool, bool) {
		if node == nil {
			if options.Emptied {
				made, _ = options.Next(nil, false, offered)
			}
			return false, false
		}
		plain := PlainScope(node)
		if node.Kind != yaml.SequenceNode || len(plain) == 0 && !options.Emptied {
			synced = false
			return false, false
		}
		next, changed := options.Next(plain, true, offered)
		if !changed {
			return false, false
		}
		kept := node.Content[:0:0]
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode && item.ShortTag() == "!!str" && !slices.Contains(next, item.Value) {
				continue
			}
			kept = append(kept, item)
		}
		for _, entry := range next {
			if !slices.Contains(plain, entry) {
				kept = append(kept, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry})
			}
		}
		if len(kept) == 0 {
			return false, true
		}
		node.Content = kept
		return true, false
	})
	if err == nil && len(made) > 0 {
		body, err = edit.RewriteYAML(ConfigFile, body, edit.KV{Path: key, Value: made})
	}
	return body, synced, err
}
