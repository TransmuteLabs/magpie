package extension

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"unicode"
)

type AgentLaunch struct {
	Env   map[string]string `json:"env,omitempty"`
	Args  map[string]string `json:"args,omitempty"`
	Model string            `json:"model,omitempty"`
	Plain []string          `json:"plain,omitempty"`
}

type AgentProfile struct {
	Target string `json:"target"`
}

type AgentLibrary struct {
	Instructions string `json:"instructions,omitempty"`
	MCP          string `json:"mcp,omitempty"`
	MCPFormat    string `json:"mcpFormat,omitempty"`
	Skills       string `json:"skills,omitempty"`
}

func (agent AgentDescriptor) Clone() AgentDescriptor {
	agent.ConfigFiles = slices.Clone(agent.ConfigFiles)
	agent.Fields = slices.Clone(agent.Fields)
	agent.Aliases = slices.Clone(agent.Aliases)
	agent.UA = slices.Clone(agent.UA)
	if agent.Launch != nil {
		launch := *agent.Launch
		launch.Env, launch.Args = maps.Clone(launch.Env), maps.Clone(launch.Args)
		launch.Plain = slices.Clone(launch.Plain)
		agent.Launch = &launch
	}
	if agent.Profile != nil {
		profile := *agent.Profile
		agent.Profile = &profile
	}
	if agent.Library != nil {
		library := *agent.Library
		agent.Library = &library
	}
	return agent
}

func agentMetadata(manifest Manifest, agent AgentDescriptor) error {
	extended := agent.Aliases != nil || agent.UA != nil || agent.Launch != nil || agent.Profile != nil || agent.Library != nil
	if extended && manifest.Contract.Version.Minor < 5 {
		return errors.New("extended agent metadata requires SDK API 1.5")
	}
	if len(agent.Aliases) > 64 || len(agent.UA) > 64 {
		return errors.New("agent identity metadata exceeds descriptor limits")
	}
	seen := map[string]bool{agent.ID: true}
	for _, alias := range agent.Aliases {
		if !validID(alias) || seen[alias] {
			return errors.New("invalid or duplicate agent alias")
		}
		seen[alias] = true
	}
	seen = map[string]bool{}
	for _, prefix := range agent.UA {
		key := strings.ToLower(prefix)
		if prefix == "" || len(prefix) > 256 || strings.TrimSpace(prefix) != prefix ||
			strings.ContainsFunc(prefix, unicode.IsControl) || seen[key] {
			return errors.New("invalid or duplicate agent user-agent prefix")
		}
		seen[key] = true
	}
	if agent.Profile != nil && (!validID(agent.Profile.Target) || agent.Profile.Target == agent.ID ||
		agent.Profile.Target == "magpie" || agent.Launch == nil && len(agent.ConfigFiles) == 0) {
		return errors.New("agent profile requires a distinct existing agent target")
	}
	if agent.Launch != nil {
		launch := agent.Launch
		if agent.Bin == "" && agent.Profile == nil || len(launch.Env) > 64 || len(launch.Args) > 64 ||
			len(launch.Plain) > 64 || launch.Model != "" && !agentFlag(launch.Model) {
			return errors.New("invalid agent launch declaration")
		}
		if launch.Model != "" && !slices.ContainsFunc(agent.Fields, func(field AgentField) bool { return field.Key == "model" }) {
			return errors.New("agent launch model flag requires a declared model field")
		}
		for key, value := range launch.Env {
			if !agentEnvName(key) || !agentLaunchText(value) {
				return errors.New("invalid agent launch environment")
			}
		}
		for key, value := range launch.Args {
			if !agentFlag(key) || !agentLaunchText(value) ||
				strings.Contains(value, "{config}") && len(agent.ConfigFiles) == 0 && agent.Profile == nil {
				return errors.New("invalid agent launch argument")
			}
		}
		seen := map[string]bool{}
		for _, value := range launch.Plain {
			if value == "" || len(value) > 128 || strings.ContainsAny(value, "*?[](){}$;|&<>`'\"\\") ||
				strings.ContainsFunc(value, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) || seen[value] {
				return errors.New("invalid or duplicate plain agent argument")
			}
			seen[value] = true
		}
	}
	if agent.Library != nil {
		library := agent.Library
		if library.Instructions == "" && library.MCP == "" && library.Skills == "" {
			return errors.New("agent library target must declare a location")
		}
		for _, location := range []string{library.Instructions, library.MCP, library.Skills} {
			if location != "" && !validHomePath(location) {
				return errors.New("agent library locations must be home-relative paths")
			}
		}
		if (library.MCP == "") != (library.MCPFormat == "") || library.MCPFormat != "" && !agentMCPFormat(library.MCPFormat) {
			return errors.New("agent MCP library target requires a format and location")
		}
	}
	return nil
}

func agentMCPFormat(value string) bool {
	switch value {
	case "alma", "antigravity", "claude", "cline", "codex", "commandcode", "copilot", "crush", "cursor",
		"devin", "gemini", "goose", "grok", "hermes", "kimi", "omp", "opencode", "zed":
		return true
	}
	return false
}

func agentFlag(value string) bool {
	if len(value) < 2 || len(value) > 128 || value[0] != '-' {
		return false
	}
	for _, char := range value[1:] {
		if char != '-' && char != '_' && (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return value != "--"
}

func agentEnvName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, char := range value {
		if char != '_' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(index == 0 || char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func agentLaunchText(value string) bool {
	return len(value) <= 8192 && !strings.ContainsFunc(value, unicode.IsControl)
}
