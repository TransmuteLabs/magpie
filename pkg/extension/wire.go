package extension

import "encoding/json"

const (
	MaxFrameBytes      = 24 << 20
	MaxChunkBytes      = 64 << 10
	MaxHostBytes       = (1 << 20) + MaxChunkBytes
	MaxConcurrent      = 32
	MaxEnvelopeBytes   = MaxFrameBytes
	MaxDataBytes       = MaxChunkBytes
	MaxConcurrentCalls = MaxConcurrent
	MaxNativeBodyBytes = 16 << 20

	KindCall       = "call"
	KindEvent      = "event"
	KindAck        = "ack"
	KindResult     = "result"
	KindCancel     = "cancel"
	KindHostCall   = "host.call"
	KindHostResult = "host.result"
)

type Scope struct {
	Plugin     string `json:"plugin,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Account    string `json:"account,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Session    string `json:"session,omitempty"`
	Operation  string `json:"operation,omitempty"`
	Attempt    uint64 `json:"attempt,omitempty"`
}

type Frame struct {
	Kind     string          `json:"kind"`
	ID       string          `json:"id"`
	Method   string          `json:"method,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Error          `json:"error,omitempty"`
	Seq      uint64          `json:"seq,omitempty"`
	Event    string          `json:"event,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Scope    Scope           `json:"scope,omitempty"`
	Auth     json.RawMessage `json:"auth,omitempty"`
	Deadline int64           `json:"deadline,omitempty"`
	Parent   string          `json:"parent,omitempty"`
}

type Call struct {
	ID     string
	Method string
	Scope  Scope
	Params json.RawMessage
	Auth   json.RawMessage
}

type Envelope = Frame

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Outcome string `json:"outcome,omitempty"`
}

func (err *Error) Error() string { return err.Message }

type RPCError = Error

const (
	MethodDescribe        = "sdk.describe"
	MethodInboundDecode   = "inbound.decode"
	MethodInboundResponse = "inbound.response"
	MethodProviderExecute = "provider.execute"
	MethodProviderModels  = "provider.models"
	MethodProviderUsage   = "provider.usage"
	MethodAuthStart       = "auth.start"
	MethodAuthFinish      = "auth.finish"
	MethodAuthRefresh     = "auth.refresh"
	MethodAuthLogout      = "auth.logout"
	MethodOperationStatus = "operation.status"
	MethodOperationCancel = "operation.cancel"
	MethodToolCall        = "tool.call"
	MethodAgentPlan       = "agent.plan"
	MethodAgentInspect    = "agent.inspect"
	MethodSessionConvert  = "session.convert"
	MethodStateGet        = "state.get"
	MethodStateCAS        = "state.cas"
	MethodNetworkProxy    = "network.proxy"
	MethodAccountReplace  = "account.replace"
)

func MethodCapability(method string) (Capability, bool) {
	switch method {
	case MethodInboundDecode, MethodInboundResponse:
		return InboundCodec, true
	case MethodProviderExecute:
		return OutboundExecutor, true
	case MethodProviderModels:
		return ModelDiscovery, true
	case MethodProviderUsage:
		return UsageReporter, true
	case MethodAuthStart, MethodAuthFinish, MethodAuthRefresh, MethodAuthLogout:
		return AccountAuth, true
	case MethodOperationStatus, MethodOperationCancel:
		return OperationControl, true
	case MethodToolCall:
		return ToolProvider, true
	case MethodAgentPlan, MethodAgentInspect:
		return AgentWiring, true
	case MethodSessionConvert:
		return SessionMigration, true
	default:
		return "", false
	}
}
