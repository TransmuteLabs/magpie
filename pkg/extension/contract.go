package extension

import "github.com/yetone/magpie/pkg/protocolext"

type APIVersion = protocolext.APIVersion
type Capability = protocolext.Capability
type Contract = protocolext.Contract

const (
	ManifestSchema                     = 1
	ManifestFile                       = "magpie-plugin.json"
	InboundCodec                       = protocolext.InboundCodec
	OutboundExecutor                   = protocolext.OutboundExecutor
	AgentWiring                        = protocolext.AgentWiring
	AccountAuth                        = protocolext.AccountAuth
	ModelDiscovery                     = protocolext.ModelDiscovery
	UsageReporter                      = protocolext.UsageReporter
	OperationControl                   = protocolext.OperationControl
	ToolProvider                       = protocolext.ToolProvider
	NativeAccountOperations            = protocolext.NativeAccountOperations
	SessionMigration        Capability = "session-migration"
	AgentProcess            Capability = "agent-process"
)

func FullContract() Contract {
	return Contract{
		Version: APIVersion{Major: 1, Minor: 15},
		Capabilities: []Capability{
			InboundCodec, OutboundExecutor, AgentWiring, AccountAuth, ModelDiscovery,
			UsageReporter, OperationControl, ToolProvider, SessionMigration, SessionRelatedInputs, AgentProcess, ModuleRPC, GatewayHooks, SessionSource, NativeAccountOperations, StorageSource, LedgerObserver, GatewayMiddleware, RequestPolicy, RequestObserver, BudgetPolicy, MaskingRules,
		},
	}
}

type Manifest struct {
	Schema            int                           `json:"schema"`
	ID                string                        `json:"id"`
	Version           string                        `json:"version"`
	Runtime           string                        `json:"runtime,omitempty"`
	Entry             string                        `json:"entry,omitempty"`
	Args              []string                      `json:"args,omitempty"`
	Contract          Contract                      `json:"contract"`
	Inbounds          []InboundDescriptor           `json:"inbounds,omitempty"`
	Providers         []ProviderDescriptor          `json:"providers,omitempty"`
	Agents            []AgentDescriptor             `json:"agents,omitempty"`
	Processes         []AgentProcessRecipe          `json:"processes,omitempty"`
	Sessions          []SessionDescriptor           `json:"sessions,omitempty"`
	SessionSources    []SessionSourceDescriptor     `json:"sessionSources,omitempty"`
	StorageSources    []StorageSourceDescriptor     `json:"storageSources,omitempty"`
	Tools             []ToolDescriptor              `json:"tools,omitempty"`
	Actions           []ActionDescriptor            `json:"actions,omitempty"`
	Services          []ServiceDescriptor           `json:"services,omitempty"`
	Compatibility     *Compatibility                `json:"compatibility,omitempty"`
	Window            *WindowDescriptor             `json:"window,omitempty"`
	Bindings          []BindingDescriptor           `json:"bindings,omitempty"`
	AuthBindings      []WindowAuthBindingDescriptor `json:"authBindings,omitempty"`
	Component         *ComponentIdentity            `json:"component,omitempty"`
	Hooks             *GatewayHookDescriptor        `json:"hooks,omitempty"`
	Middleware        *GatewayMiddlewareDescriptor  `json:"middleware,omitempty"`
	CatalogAccess     *CatalogAccessDescriptor      `json:"catalogAccess,omitempty"`
	BudgetPolicies    []BudgetPolicyDescriptor      `json:"budgetPolicies,omitempty"`
	MaskingRuleSets   []MaskingRuleSet              `json:"maskingRuleSets,omitempty"`
	RequestObserver   *RequestObservationDescriptor `json:"requestObserver,omitempty"`
	Ledger            *LedgerObservationDescriptor  `json:"ledger,omitempty"`
	RoutingStrategies []RoutingStrategyDescriptor   `json:"routingStrategies,omitempty"`
	SearchSources     []SearchSourceDescriptor      `json:"searchSources,omitempty"`
	AccountOperations []AccountOperationDescriptor  `json:"accountOperations,omitempty"`
	Activation        []string                      `json:"activation,omitempty"`
}

type InboundDescriptor struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type ProviderDescriptor struct {
	ID             string                      `json:"id"`
	Name           string                      `json:"name"`
	Protocols      []protocolext.Protocol      `json:"protocols"`
	Models         []Model                     `json:"models,omitempty"`
	Operations     []ProviderOperation         `json:"operations,omitempty"`
	Calls          bool                        `json:"calls,omitempty"`
	Checkin        *AccountCheckinDescriptor   `json:"checkin,omitempty"`
	KeepAlive      *AccountKeepAliveDescriptor `json:"keepAlive,omitempty"`
	Options        []ProviderOption            `json:"options,omitempty"`
	AccountTasks   []AccountTaskDescriptor     `json:"accountTasks,omitempty"`
	MaxConcurrency *int                        `json:"maxConcurrency,omitempty"`
	MaxAccounts    *int                        `json:"maxAccounts,omitempty"`
}

type AgentDescriptor struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Bin         string        `json:"bin,omitempty"`
	ConfigFiles []string      `json:"configFiles,omitempty"`
	Fields      []AgentField  `json:"fields,omitempty"`
	Aliases     []string      `json:"aliases,omitempty"`
	UA          []string      `json:"ua,omitempty"`
	Launch      *AgentLaunch  `json:"launch,omitempty"`
	Profile     *AgentProfile `json:"profile,omitempty"`
	Library     *AgentLibrary `json:"library,omitempty"`
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
