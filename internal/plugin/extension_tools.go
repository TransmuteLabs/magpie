package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const ExtensionToolPrefix = "extension:"

type extensionToolRuntime interface {
	List() ([]extension.Installed, error)
	Call(context.Context, string, string, sdk.Scope, any, func(extension.Event) error) (json.RawMessage, error)
}

var extensionToolsRuntime = func() extensionToolRuntime { return extension.Default() }

var legacyToolPlugins = Plugins

func ExtensionToolID(spec string) (string, bool) {
	id, found := strings.CutPrefix(spec, ExtensionToolPrefix)
	return id, found && sdk.ValidID(id)
}

func extensionTools(entries []extension.Installed, enabledOnly bool) []PluginTools {
	var tools []PluginTools
	for _, entry := range entries {
		if enabledOnly && entry.Disabled || !entry.Manifest.Contract.Has(sdk.ToolProvider) {
			continue
		}
		group := PluginTools{Spec: ExtensionToolPrefix + entry.Manifest.ID}
		for _, descriptor := range entry.Manifest.Tools {
			group.Tools = append(group.Tools, Tool{
				Name: descriptor.Name, Description: descriptor.Description, Schema: bytes.Clone(descriptor.InputSchema),
			})
		}
		if len(group.Tools) > 0 {
			tools = append(tools, group)
		}
	}
	return tools
}

func ReadList() (List, error) {
	list, err := readLegacyToolList()
	if err != nil {
		return List{}, err
	}
	entries, err := extensionToolsRuntime().List()
	if err != nil {
		return List{}, err
	}
	for _, entry := range entries {
		list.Plugins = append(list.Plugins, Entry{Spec: ExtensionToolPrefix + entry.Manifest.ID, Off: entry.Disabled})
	}
	return list, nil
}

func ToolsOn() (int, error) {
	list, err := readLegacyToolList()
	if err != nil {
		return 0, err
	}
	count, err := legacyToolsOn(list)
	if err != nil {
		return 0, err
	}
	entries, err := extensionToolsRuntime().List()
	if err != nil {
		return 0, err
	}
	return count + len(extensionTools(entries, true)), nil
}

func ToolNames() map[string][]string {
	names, err := legacyToolNames()
	if err != nil {
		return nil
	}
	entries, err := extensionToolsRuntime().List()
	if err != nil {
		return nil
	}
	for _, group := range extensionTools(entries, false) {
		if names == nil {
			names = map[string][]string{}
		}
		for _, tool := range group.Tools {
			names[group.Spec] = append(names[group.Spec], tool.Name)
		}
		sort.Strings(names[group.Spec])
	}
	return names
}

// RunExtensionTool keeps MCP content blocks intact for the gateway bridge.
func RunExtensionTool(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
	var result sdk.ToolResult
	id, valid := ExtensionToolID(spec)
	if !valid {
		return result, errors.New("invalid extension tool spec")
	}
	if err := netproxy.Held(ctx); err != nil {
		return result, err
	}
	if len(bytes.TrimSpace(args)) == 0 || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		args = json.RawMessage(`{}`)
	}
	var arguments map[string]json.RawMessage
	if len(args) > 4<<20 || json.Unmarshal(args, &arguments) != nil || arguments == nil {
		return result, errors.New("extension tool arguments must be a bounded JSON object")
	}
	if _, reserved := arguments["_magpie"]; reserved {
		return result, errors.New("extension tool account context must be resolved by the host")
	}
	scope := extension.ToolScopeFrom(ctx)
	if scope.Plugin != "" && scope.Plugin != id {
		return result, errors.New("extension tool scope belongs to another module")
	}
	scope.Plugin = id
	manager := extensionToolsRuntime()
	entries, err := manager.List()
	if err != nil {
		return result, err
	}
	declared := false
	for _, group := range extensionTools(entries, true) {
		if group.Spec == spec {
			for _, tool := range group.Tools {
				declared = declared || tool.Name == name
			}
		}
	}
	if !declared {
		return result, errors.New("extension is disabled, removed, or does not declare this tool")
	}
	ctx, cancel := context.WithTimeout(ctx, ToolTimeout)
	defer cancel()
	body, err := manager.Call(ctx, id, sdk.MethodToolCall, scope, sdk.ToolRequest{ID: name, Arguments: bytes.Clone(args)}, nil)
	if err != nil {
		return result, err
	}
	if len(body) > sdk.MaxFrameBytes || json.Unmarshal(body, &result) != nil || !json.Valid(result.Content) {
		return sdk.ToolResult{}, errors.New("extension returned an invalid tool result")
	}
	return result, nil
}

func extensionToolText(result sdk.ToolResult) (string, error) {
	var text string
	if json.Unmarshal(result.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(result.Content, &blocks) == nil && blocks != nil {
			var texts []string
			allText := true
			for _, block := range blocks {
				allText = allText && block.Type == "text"
				texts = append(texts, block.Text)
			}
			if allText {
				text = strings.Join(texts, "\n")
			} else {
				text = string(result.Content)
			}
		} else {
			text = string(result.Content)
		}
	}
	if result.IsError {
		return "", errors.New(text)
	}
	return text, nil
}
