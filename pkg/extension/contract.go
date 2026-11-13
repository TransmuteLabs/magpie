package extension

import "github.com/yetone/magpie/pkg/protocolext"

type APIVersion = protocolext.APIVersion
type Capability = protocolext.Capability
type Contract = protocolext.Contract

const (
	ManifestSchema              = 1
	ManifestFile                = "magpie-plugin.json"
	InboundCodec                = protocolext.InboundCodec
	OutboundExecutor            = protocolext.OutboundExecutor
	AgentWiring                 = protocolext.AgentWiring
	AccountAuth                 = protocolext.AccountAuth
	ModelDiscovery              = protocolext.ModelDiscovery
	UsageReporter               = protocolext.UsageReporter
	OperationControl            = protocolext.OperationControl
	ToolProvider                = protocolext.ToolProvider
	SessionMigration Capability = "session-migration"
)

func FullContract() Contract {
	return Contract{
		Version: APIVersion{Major: 1, Minor: 1},
		Capabilities: []Capability{
			InboundCodec, OutboundExecutor, AgentWiring, AccountAuth, ModelDiscovery,
			UsageReporter, OperationControl, ToolProvider, SessionMigration, ModuleRPC,
		},
	}
}

type Manifest struct {
	Schema        int                  `json:"schema"`
	ID            string               `json:"id"`
	Version       string               `json:"version"`
	Runtime       string               `json:"runtime,omitempty"`
	Entry         string               `json:"entry,omitempty"`
	Args          []string             `json:"args,omitempty"`
	Contract      Contract             `json:"contract"`
	Inbounds      []InboundDescriptor  `json:"inbounds,omitempty"`
	Providers     []ProviderDescriptor `json:"providers,omitempty"`
	Agents        []AgentDescriptor    `json:"agents,omitempty"`
	Sessions      []SessionDescriptor  `json:"sessions,omitempty"`
	Tools         []ToolDescriptor     `json:"tools,omitempty"`
	Actions       []ActionDescriptor   `json:"actions,omitempty"`
	Services      []ServiceDescriptor  `json:"services,omitempty"`
	Compatibility *Compatibility       `json:"compatibility,omitempty"`
	Window        *WindowDescriptor    `json:"window,omitempty"`
	Bindings      []BindingDescriptor  `json:"bindings,omitempty"`
	Component     *ComponentIdentity   `json:"component,omitempty"`
}

type InboundDescriptor struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type ProviderDescriptor struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Protocols []protocolext.Protocol `json:"protocols"`
	Models    []Model                `json:"models,omitempty"`
}

type AgentDescriptor struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Bin         string       `json:"bin,omitempty"`
	ConfigFiles []string     `json:"configFiles,omitempty"`
	Fields      []AgentField `json:"fields,omitempty"`
}

type AgentField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type SessionDescriptor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	OutputDir string `json:"outputDir,omitempty"`
}
