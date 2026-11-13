package extension

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

func (manifest Manifest) Public() Manifest {
	snapshot := cloneManifest(manifest)
	snapshot.Runtime, snapshot.Entry, snapshot.Args = "", "", nil
	snapshot.Services, snapshot.Window, snapshot.Bindings, snapshot.Compatibility = nil, nil, nil, nil
	snapshot.Actions = slices.DeleteFunc(snapshot.Actions, func(action ActionDescriptor) bool {
		return actionOwner(action) != componentID(manifest)
	})
	return snapshot
}

// Clone retains the complete installation authority in an independent copy.
func (manifest Manifest) Clone() Manifest { return cloneManifest(manifest) }

func cloneManifest(manifest Manifest) Manifest {
	manifest.Args = slices.Clone(manifest.Args)
	manifest.Contract.Capabilities = slices.Clone(manifest.Contract.Capabilities)
	manifest.Inbounds = slices.Clone(manifest.Inbounds)
	manifest.Providers = slices.Clone(manifest.Providers)
	for index := range manifest.Providers {
		provider := &manifest.Providers[index]
		provider.Protocols = slices.Clone(provider.Protocols)
		provider.Models = slices.Clone(provider.Models)
		for model := range provider.Models {
			provider.Models[model].Protocols = slices.Clone(provider.Models[model].Protocols)
			provider.Models[model].Efforts = slices.Clone(provider.Models[model].Efforts)
			if provider.Models[model].Price != nil {
				price := *provider.Models[model].Price
				provider.Models[model].Price = &price
			}
		}
	}
	manifest.Agents = slices.Clone(manifest.Agents)
	for index := range manifest.Agents {
		manifest.Agents[index].ConfigFiles = slices.Clone(manifest.Agents[index].ConfigFiles)
		manifest.Agents[index].Fields = slices.Clone(manifest.Agents[index].Fields)
	}
	manifest.Sessions = slices.Clone(manifest.Sessions)
	manifest.Tools = slices.Clone(manifest.Tools)
	for index := range manifest.Tools {
		manifest.Tools[index].InputSchema = slices.Clone(manifest.Tools[index].InputSchema)
	}
	manifest.Actions = slices.Clone(manifest.Actions)
	for index := range manifest.Actions {
		manifest.Actions[index].InputSchema = slices.Clone(manifest.Actions[index].InputSchema)
	}
	manifest.Services = slices.Clone(manifest.Services)
	for index := range manifest.Services {
		service := &manifest.Services[index]
		service.Args = slices.Clone(service.Args)
		service.Methods = slices.Clone(service.Methods)
		service.DependsOn = slices.Clone(service.DependsOn)
		service.Ready.HTTPEndpoints = slices.Clone(service.Ready.HTTPEndpoints)
	}
	manifest.Bindings = slices.Clone(manifest.Bindings)
	if manifest.Compatibility != nil {
		value := *manifest.Compatibility
		value.Platforms = slices.Clone(value.Platforms)
		manifest.Compatibility = &value
	}
	if manifest.Window != nil {
		value := *manifest.Window
		manifest.Window = &value
	}
	if manifest.Component != nil {
		value := *manifest.Component
		manifest.Component = &value
	}
	return manifest
}

func (manifest Manifest) Validate() error {
	if manifest.Schema != ManifestSchema {
		return fmt.Errorf("unsupported extension manifest schema %d", manifest.Schema)
	}
	if !validID(manifest.ID) {
		return fmt.Errorf("invalid extension id %q", manifest.ID)
	}
	if !validVersion(manifest.Version) {
		return fmt.Errorf("invalid extension version %q", manifest.Version)
	}
	if manifest.Runtime != "exec" && manifest.Runtime != "bun" && manifest.Runtime != "native" {
		return fmt.Errorf("unsupported extension runtime %q", manifest.Runtime)
	}
	if manifest.Entry == "" || strings.HasPrefix(manifest.Entry, "/") || path.Clean(manifest.Entry) != manifest.Entry ||
		strings.ContainsAny(manifest.Entry, "\\:\x00") || manifest.Entry == "." || manifest.Entry == ".." || strings.HasPrefix(manifest.Entry, "../") {
		return fmt.Errorf("extension entry must be a clean relative package path")
	}
	if len(manifest.Entry) > 1024 || strings.ContainsFunc(manifest.Entry, unicode.IsControl) {
		return fmt.Errorf("invalid extension entry")
	}
	if len(manifest.Args) > 128 {
		return fmt.Errorf("too many extension arguments")
	}
	if manifest.Runtime == "native" && len(manifest.Args) != 0 {
		return fmt.Errorf("native extensions do not take process arguments")
	}
	for _, argument := range manifest.Args {
		if len(argument) > MaxChunkBytes || strings.ContainsRune(argument, 0) {
			return fmt.Errorf("invalid extension argument")
		}
	}
	if err := FullContract().Check(manifest.Contract); err != nil {
		return err
	}
	if len(manifest.Inbounds)+len(manifest.Providers)+len(manifest.Agents)+len(manifest.Sessions)+len(manifest.Tools) > 1024 {
		return fmt.Errorf("too many extension descriptors")
	}
	paths := map[string]bool{}
	ids := map[string]bool{}
	for _, inbound := range manifest.Inbounds {
		if !manifest.Contract.Has(InboundCodec) || !validID(inbound.ID) || ids[inbound.ID] {
			return fmt.Errorf("invalid or duplicate inbound descriptor %q", inbound.ID)
		}
		if !validEndpoint(inbound.Path) || reservedEndpoint(inbound.Path) || paths[inbound.Path] {
			return fmt.Errorf("invalid, reserved or duplicate inbound path %q", inbound.Path)
		}
		ids[inbound.ID], paths[inbound.Path] = true, true
	}
	ids = map[string]bool{}
	for _, provider := range manifest.Providers {
		if !validID(provider.ID) || ids[provider.ID] || !validName(provider.Name) || len(provider.Protocols) == 0 {
			return fmt.Errorf("invalid or duplicate provider descriptor %q", provider.ID)
		}
		if !manifest.Contract.Has(OutboundExecutor) && !manifest.Contract.Has(ModelDiscovery) {
			return fmt.Errorf("provider descriptor requires an executor or discovery capability")
		}
		ids[provider.ID] = true
		protocols := map[string]bool{}
		for _, protocol := range provider.Protocols {
			value := string(protocol)
			if !validProtocol(value) || protocols[value] {
				return fmt.Errorf("unsupported or duplicate provider protocol %q", value)
			}
			protocols[value] = true
		}
		models := map[string]bool{}
		for _, model := range provider.Models {
			if model.ID == "" || len(model.ID) > 300 || strings.ContainsFunc(model.ID, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) || models[model.ID] {
				return fmt.Errorf("invalid or duplicate model %q", model.ID)
			}
			if model.ContextWindow < 0 || model.MaxOutput < 0 {
				return fmt.Errorf("invalid model limits")
			}
			if model.Price != nil {
				if model.Price.Currency != "" && model.Price.Currency != "USD" {
					return fmt.Errorf("model prices must use USD per million tokens")
				}
				for _, rate := range []float64{model.Price.Input, model.Price.Output, model.Price.CacheRead, model.Price.CacheWrite, model.Price.CacheWrite1h} {
					if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
						return fmt.Errorf("model price rates must be finite and nonnegative")
					}
				}
			}
			if len(model.Efforts) > 7 {
				return fmt.Errorf("too many model effort levels")
			}
			efforts := map[string]bool{}
			for _, effort := range model.Efforts {
				switch effort {
				case "none", "minimal", "low", "medium", "high", "xhigh", "max":
				default:
					return fmt.Errorf("unsupported model effort level %q", effort)
				}
				if efforts[effort] {
					return fmt.Errorf("duplicate model effort level %q", effort)
				}
				efforts[effort] = true
			}
			for _, protocol := range model.Protocols {
				if !protocols[string(protocol)] {
					return fmt.Errorf("model protocol was not declared by its provider")
				}
			}
			models[model.ID] = true
		}
	}
	ids = map[string]bool{}
	for _, agent := range manifest.Agents {
		if !manifest.Contract.Has(AgentWiring) || !validID(agent.ID) || !validName(agent.Name) || ids[agent.ID] {
			return fmt.Errorf("invalid or duplicate agent descriptor %q", agent.ID)
		}
		if agent.Bin != "" && (!validID(agent.Bin) || strings.ContainsAny(agent.Bin, "/\\")) {
			return fmt.Errorf("agent binary must be an executable basename")
		}
		if len(agent.ConfigFiles) > 128 || len(agent.Fields) > 128 {
			return fmt.Errorf("agent wiring metadata exceeds descriptor limits")
		}
		files, fields := map[string]bool{}, map[string]bool{}
		for _, file := range agent.ConfigFiles {
			folded := strings.ToLower(file)
			if !validHomePath(file) || files[folded] {
				return fmt.Errorf("invalid or duplicate agent config path %q", file)
			}
			files[folded] = true
		}
		for _, field := range agent.Fields {
			if !validID(field.Key) || !validName(field.Label) || fields[field.Key] {
				return fmt.Errorf("invalid or duplicate agent field %q", field.Key)
			}
			fields[field.Key] = true
		}
		ids[agent.ID] = true
	}
	ids = map[string]bool{}
	for _, session := range manifest.Sessions {
		if !manifest.Contract.Has(SessionMigration) || !validID(session.ID) || !validName(session.Name) || ids[session.ID] {
			return fmt.Errorf("invalid or duplicate session descriptor %q", session.ID)
		}
		if session.From != "" || session.To != "" || session.OutputDir != "" {
			if !validID(session.From) || !validID(session.To) || session.From == session.To || !validHomePath(session.OutputDir) {
				return fmt.Errorf("session route requires distinct formats and a home-relative output directory")
			}
		}
		ids[session.ID] = true
	}
	ids = map[string]bool{}
	for _, tool := range manifest.Tools {
		var schema any
		if !manifest.Contract.Has(ToolProvider) || !validID(tool.Name) || ids[tool.Name] || len(tool.InputSchema) > MaxChunkBytes ||
			json.Unmarshal(tool.InputSchema, &schema) != nil {
			return fmt.Errorf("invalid or duplicate tool descriptor %q", tool.Name)
		}
		switch schema.(type) {
		case map[string]any, bool:
		default:
			return fmt.Errorf("tool input schema must be a JSON schema object or boolean")
		}
		ids[tool.Name] = true
	}
	if manifest.Contract.Has(InboundCodec) && len(manifest.Inbounds) == 0 {
		return fmt.Errorf("inbound capability requires an inbound descriptor")
	}
	if manifest.Contract.Has(ToolProvider) && len(manifest.Tools) == 0 {
		return fmt.Errorf("tool capability requires a tool descriptor")
	}
	if manifest.Contract.Has(ModelDiscovery) && len(manifest.Providers) == 0 && !manifest.Contract.Has(InboundCodec) {
		return fmt.Errorf("model discovery requires providers or an inbound codec")
	}
	if len(manifest.Providers) == 0 && (manifest.Contract.Has(OutboundExecutor) || manifest.Contract.Has(AccountAuth) || manifest.Contract.Has(UsageReporter)) {
		return fmt.Errorf("provider capabilities require a provider descriptor")
	}
	if manifest.Contract.Has(AgentWiring) && len(manifest.Agents) == 0 {
		return fmt.Errorf("agent wiring requires an agent descriptor")
	}
	if manifest.Contract.Has(SessionMigration) && len(manifest.Sessions) == 0 {
		return fmt.Errorf("session migration requires a session descriptor")
	}
	return manifest.validateComposite()
}

func ValidID(value string) bool {
	return validID(value)
}

func validID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if index == 0 || char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func validName(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && !strings.ContainsFunc(value, unicode.IsControl)
}

func validVersion(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	core, build, hasBuild := strings.Cut(value, "+")
	if hasBuild && !validVersionIdentifiers(build, false) {
		return false
	}
	core, prerelease, hasPrerelease := strings.Cut(core, "-")
	if hasPrerelease && !validVersionIdentifiers(prerelease, true) {
		return false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 64); err != nil || strings.ContainsAny(part, "+-") {
			return false
		}
	}
	return true
}

func validVersionIdentifiers(value string, prerelease bool) bool {
	for _, part := range strings.Split(value, ".") {
		if part == "" {
			return false
		}
		numeric := true
		for _, char := range part {
			if char < '0' || char > '9' {
				numeric = false
			}
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
		if prerelease && numeric && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func validEndpoint(endpoint string) bool {
	return endpoint != "/" && strings.HasPrefix(endpoint, "/") && path.Clean(endpoint) == endpoint &&
		!strings.ContainsAny(endpoint, "{}?%#\\") &&
		!strings.ContainsFunc(endpoint, func(char rune) bool { return unicode.IsControl(char) || unicode.IsSpace(char) })
}

func reservedEndpoint(endpoint string) bool {
	for _, prefix := range []string{"/api", "/_magpie", "/agent", "/v1", "/v1beta", "/models", "/chat/completions",
		"/responses", "/messages", "/images", "/embeddings", "/rerank", "/decisions", "/audio", "/videos", "/mcp",
		"/backend-api", "/muse-code", "/alpha/search", "/magpie-ca.crt"} {
		if endpoint == prefix || strings.HasPrefix(endpoint, prefix+"/") {
			return true
		}
	}
	return strings.HasPrefix(endpoint, "/wsl-")
}

func validProtocol(protocol string) bool {
	switch protocol {
	case "chat", "responses", "messages", "anthropic", "gemini":
		return true
	default:
		return false
	}
}

func RequiredMethods(manifest Manifest) []string {
	var methods []string
	for _, capability := range manifest.Contract.Capabilities {
		switch capability {
		case InboundCodec:
			methods = append(methods, MethodInboundDecode, MethodInboundResponse)
		case OutboundExecutor:
			methods = append(methods, MethodProviderExecute)
		case ModelDiscovery:
			if len(manifest.Providers) > 0 {
				methods = append(methods, MethodProviderModels)
			}
		case AccountAuth:
			methods = append(methods, MethodAuthStart, MethodAuthFinish, MethodAuthRefresh, MethodAuthLogout)
		case UsageReporter:
			methods = append(methods, MethodProviderUsage)
		case OperationControl:
			methods = append(methods, MethodOperationStatus, MethodOperationCancel)
		case ToolProvider:
			methods = append(methods, MethodToolCall)
		case AgentWiring:
			methods = append(methods, MethodAgentPlan)
			for _, agent := range manifest.Agents {
				if agent.Bin != "" || len(agent.ConfigFiles) != 0 || len(agent.Fields) != 0 {
					methods = append(methods, MethodAgentInspect)
					break
				}
			}
		case SessionMigration:
			methods = append(methods, MethodSessionConvert)
		case ModuleRPC:
			methods = append(methods, MethodModuleCall)
		}
	}
	slices.Sort(methods)
	return methods
}

// Paths in ordinary agent/session descriptors are exact locations below HOME.
// Explicit action callers can approve a different root separately.
func validHomePath(name string) bool {
	return name != "" && len(name) <= 1024 && name != "." && name != ".." &&
		!strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") &&
		path.Clean(name) == name && !strings.ContainsAny(name, "\\:\x00*?[]") &&
		!strings.ContainsFunc(name, unicode.IsControl) &&
		!strings.Contains(strings.ToLower(name), ".magpie-extension")
}
