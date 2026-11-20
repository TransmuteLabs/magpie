package hermes

import (
	"errors"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
	"gopkg.in/yaml.v3"
)

const (
	providerID        = "magpie"
	AnthropicProvider = providerID + "-anthropic"
	messagesMode      = "anthropic_messages"
)

var defaultModes = map[string]string{providerID: "chat_completions", AnthropicProvider: messagesMode}

var modeAliases = map[string]string{
	"openai": "chat_completions", "openai_chat": "chat_completions", "openai-chat": "chat_completions",
	"chat-completions": "chat_completions", "chatcompletions": "chat_completions",
	"responses": "codex_responses", "openai_responses": "codex_responses", "openai-responses": "codex_responses",
	"anthropic": messagesMode, "anthropic-messages": messagesMode, "messages": messagesMode,
	"bedrock": "bedrock_converse", "bedrock-converse": "bedrock_converse",
}

func Mode(provider, apiMode, transport string) (string, bool) {
	mode := strings.TrimSpace(apiMode)
	if mode == "" {
		mode = strings.TrimSpace(transport)
	}
	if canonical, ok := modeAliases[strings.ToLower(mode)]; ok {
		mode = canonical
	}
	if mode == "" || mode == defaultModes[provider] {
		return defaultModes[provider], false
	}
	return mode, true
}

func Base(gateway, mode string) string {
	if mode == messagesMode {
		return gateway
	}
	return gateway + "/v1"
}

func ProxyPlan(gateway string, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	for index, file := range settings.Files {
		providers, err := edit.ReadYAMLNode(file.Path, file.Body, "providers")
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		var changes []edit.KV
		for _, name := range []string{providerID, AnthropicProvider} {
			entry := child(providers, name)
			if entry == nil || entry.Kind != yaml.MappingNode {
				if index == 0 && name == providerID {
					return sdk.AgentPlan{}, errors.New("connect Hermes to magpie first: pick one of magpie's models for it on the Agents page")
				}
				continue
			}
			mode, _ := Mode(name, scalar(entry, "api_mode"), scalar(entry, "transport"))
			changes = append(changes, edit.KV{Path: "providers." + name + ".base_url", Value: Base(gateway, mode)})
		}
		if len(changes) == 0 {
			continue
		}
		body, err := edit.RewriteYAML(file.Path, file.Body, changes...)
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: body})
	}
	return plan, nil
}

func child(node *yaml.Node, key string) *yaml.Node {
	if node != nil && node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				return node.Content[index+1]
			}
		}
	}
	return nil
}

func scalar(node *yaml.Node, key string) string {
	if value := child(node, key); value != nil && value.Kind == yaml.ScalarNode {
		return value.Value
	}
	return ""
}
