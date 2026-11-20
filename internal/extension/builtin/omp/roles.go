package omp

import (
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
)

var Efforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

var ModelReferenceKeys = []string{"modelRoles", "retry.fallbackChains", "task.agentModelOverrides", "enabledModels"}

const AllGatewayModels = "magpie/**"

func MapReferences(value string, mapReference func(string) string) string {
	parts := strings.Split(value, ",")
	for i, part := range parts {
		lead := len(part) - len(strings.TrimLeft(part, " \t"))
		parts[i] = part[:lead] + mapReference(part[lead:])
	}
	return strings.Join(parts, ",")
}

func Level(value string) (model, level string) {
	if i := strings.LastIndexByte(value, ':'); i > 0 && !strings.Contains(value, ",") {
		if effort := value[i+1:]; effort == "off" || effort == "auto" || slices.Contains(Efforts, effort) {
			return value[:i], value[i:]
		}
	}
	return value, ""
}

func SplitRole(value string) (model, level string, one bool) {
	if strings.Contains(value, ",") {
		return value, "", false
	}
	model, level = Level(value)
	return model, level, true
}

func RoleOnMagpie(value string) bool {
	_, _, one := SplitRole(value)
	return one && strings.HasPrefix(value, "magpie/")
}

func YAMLList(value string) bool {
	return strings.HasPrefix(value, "[") || strings.HasPrefix(value, "- ")
}

type RoleOptions struct {
	Name, Value, Original string
	AdvisorOwned          bool
	RoutedModel           func(string) bool
	UpdateEnabledModels   func(string) func(*yaml.Node) (bool, bool)
}

type RoleResult struct {
	Original       string
	Remember       bool
	Forget         bool
	ClearedScope   bool
	AdvisorEnabled bool
	AdvisorReset   bool
}

func rewriteRole(options RoleOptions, raw []byte) ([]byte, CatalogMode, bool, RoleResult, error) {
	var result RoleResult
	key := "modelRoles." + options.Name
	node, err := edit.ReadYAMLNode(ConfigFile, raw, key)
	if err != nil {
		return nil, 0, false, result, err
	}
	current := roleValue(node)
	value, reset := options.Value, options.Value == ""
	if reset {
		if RoleOnMagpie(current) {
			value = options.Original
			result.Forget = true
		}
	} else if _, level, one := SplitRole(value); one && level == "" {
		if model, previous := Level(current); previous != "" && model != value {
			value += previous
		}
	}
	model, _, one := SplitRole(value)
	ref, prefixed := strings.CutPrefix(model, "magpie/")
	restoredList := reset && YAMLList(value)
	routed := !restoredList && one && prefixed && options.RoutedModel(ref)
	if routed && !RoleOnMagpie(current) {
		result.Original, result.Remember = current, true
		if node != nil && node.Kind != yaml.ScalarNode {
			body, err := yaml.Marshal(node)
			if err != nil {
				return nil, 0, false, result, edit.AtFile(ConfigFile, err)
			}
			result.Original = strings.TrimSuffix(string(body), "\n")
		}
	}
	body := raw
	switch {
	case value == "":
		body, err = edit.RewriteYAMLWithout(ConfigFile, body, key)
	case restoredList:
		body, err = edit.RewriteYAML(ConfigFile, body, edit.KV{Path: key, Value: edit.YAMLText(value)})
	default:
		result.Forget = result.Forget || !routed
		body, err = edit.RewriteYAML(ConfigFile, body, edit.KV{Path: key, Value: value})
	}
	if err != nil {
		return nil, 0, false, result, err
	}
	mode := WriteCatalog
	if !routed {
		used, err := retainsCatalog(body)
		if err != nil {
			return nil, 0, false, result, err
		}
		if !used {
			mode, result.ClearedScope = DropCatalog, true
			body, err = edit.RewriteYAMLNode(ConfigFile, body, "enabledModels", dropScope)
			if err != nil {
				return nil, 0, false, result, err
			}
		}
	}
	if options.Name == "default" && !reset && one && model != "" {
		body, err = edit.RewriteYAMLNode(ConfigFile, body, "enabledModels", options.UpdateEnabledModels(model))
	}
	if options.Name == "advisor" {
		body, err = rewriteAdvisor(body, reset, options.AdvisorOwned, &result)
	}
	return body, mode, routed, result, err
}

func roleValue(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value
	}
	if node.Kind != yaml.SequenceNode {
		return ""
	}
	var values []string
	for _, item := range node.Content {
		if item.Kind == yaml.ScalarNode {
			values = append(values, item.Value)
		}
	}
	return strings.Join(values, ",")
}

func rewriteAdvisor(raw []byte, reset, owned bool, result *RoleResult) ([]byte, error) {
	const key = "advisor.enabled"
	node, err := edit.ReadYAMLNode(ConfigFile, raw, key)
	if err != nil {
		return nil, err
	}
	on := node != nil && node.Kind == yaml.ScalarNode
	if reset {
		result.AdvisorReset = owned
		if owned && on && node.Value == "true" {
			return edit.RewriteYAMLWithout(ConfigFile, raw, key)
		}
	} else if !on || node.Value == "" {
		result.AdvisorEnabled = true
		return edit.RewriteYAML(ConfigFile, raw, edit.KV{Path: key, Value: true})
	}
	return raw, nil
}

func retainsCatalog(raw []byte) (bool, error) {
	used := false
	read := func(value string) string {
		MapReferences(value, func(ref string) string {
			used = used || strings.HasPrefix(ref, "magpie/")
			return ref
		})
		return value
	}
	keys := ModelReferenceKeys[:len(ModelReferenceKeys)-1]
	if _, err := edit.RewriteYAMLStrings(ConfigFile, raw, keys, read); err != nil || used {
		return used, err
	}
	scope, err := edit.ReadYAMLNode(ConfigFile, raw, "enabledModels")
	if err != nil || scope == nil || scope.Kind != yaml.SequenceNode {
		return false, err
	}
	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode {
			read(node.Value)
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	for _, node := range scope.Content {
		if node.Kind == yaml.MappingNode {
			walk(node)
		}
	}
	return used, nil
}

func dropScope(node *yaml.Node) (changed, remove bool) {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false, false
	}
	kept := node.Content[:0:0]
	for _, item := range node.Content {
		value := strings.ToLower(strings.TrimSpace(item.Value))
		if item.Kind == yaml.ScalarNode && (value == AllGatewayModels ||
			!strings.ContainsAny(item.Value, "*?[") && strings.HasPrefix(value, "magpie/")) {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == len(node.Content) {
		return false, false
	}
	if len(kept) == 0 {
		return false, true
	}
	node.Content = kept
	return true, false
}
