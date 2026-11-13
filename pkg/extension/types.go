package extension

import (
	"encoding/json"
	"net/http"

	"github.com/yetone/magpie/pkg/protocolext"
)

type NativeRequest struct {
	ID         string      `json:"id"`
	ResponseID string      `json:"responseId"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`
}

type DecodedRequest struct {
	Protocol  string          `json:"protocol"`
	Body      []byte          `json:"body,omitempty"`
	Session   string          `json:"session,omitempty"`
	TimeoutMs int64           `json:"timeoutMs,omitempty"`
	RequestID string          `json:"requestId,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type ResponseEvent struct {
	ID         string      `json:"id"`
	ResponseID string      `json:"responseId"`
	Action     string      `json:"action"`
	Status     int         `json:"status,omitempty"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`
	Error      *RPCError   `json:"error,omitempty"`
}

type HeadersEvent struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers,omitempty"`
}

type ExecuteRequest struct {
	Protocol    string          `json:"protocol"`
	Body        []byte          `json:"body,omitempty"`
	Headers     http.Header     `json:"headers,omitempty"`
	Model       string          `json:"model"`
	OperationID string          `json:"operationId,omitempty"`
	Credentials json.RawMessage `json:"credentials,omitempty"`
	Options     json.RawMessage `json:"options,omitempty"`
}

type ProviderRequest = ExecuteRequest

type ExecuteResult struct {
	Status  int         `json:"status,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Usage   *Usage      `json:"usage,omitempty"`
	Outcome string      `json:"outcome,omitempty"`
}

type AcceptedEvent struct {
	OperationID string          `json:"operationId"`
	Reference   json.RawMessage `json:"reference,omitempty"`
}

type Model struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name,omitempty"`
	Protocols     []protocolext.Protocol `json:"protocols,omitempty"`
	ContextWindow int64                  `json:"contextWindow,omitempty"`
	MaxOutput     int64                  `json:"maxOutput,omitempty"`
	Tools         bool                   `json:"tools,omitempty"`
	Vision        bool                   `json:"vision,omitempty"`
	Reasoning     bool                   `json:"reasoning,omitempty"`
	Price         *ModelPrice            `json:"price,omitempty"`
	Efforts       []string               `json:"efforts,omitempty"`
}

type ModelPrice struct {
	Currency     string  `json:"currency,omitempty"`
	Input        float64 `json:"input,omitempty"`
	Output       float64 `json:"output,omitempty"`
	CacheRead    float64 `json:"cacheRead,omitempty"`
	CacheWrite   float64 `json:"cacheWrite,omitempty"`
	CacheWrite1h float64 `json:"cacheWrite1h,omitempty"`
}

type ModelsRequest struct {
	Credentials json.RawMessage `json:"credentials,omitempty"`
}

type ModelsResult struct {
	Models []Model `json:"models"`
}

type AuthStartRequest struct {
	Method      string          `json:"method"`
	RedirectURI string          `json:"redirectUri,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
}

type AuthStartResult struct {
	URL          string          `json:"url,omitempty"`
	UserCode     string          `json:"userCode,omitempty"`
	PollAfterMs  int64           `json:"pollAfterMs,omitempty"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
}

type AuthFinishRequest struct {
	Code         string          `json:"code,omitempty"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
}

type AuthRefreshRequest struct {
	Credentials json.RawMessage `json:"credentials"`
}

type AuthLogoutRequest struct {
	Credentials json.RawMessage `json:"credentials,omitempty"`
}

type AuthResult struct {
	ID          string          `json:"id,omitempty"`
	Name        string          `json:"name,omitempty"`
	Credentials json.RawMessage `json:"credentials,omitempty"`
	ExpiresAt   int64           `json:"expiresAt,omitempty"`
	Pending     bool            `json:"pending,omitempty"`
}

type Usage struct {
	InputTokens      int64   `json:"inputTokens,omitempty"`
	OutputTokens     int64   `json:"outputTokens,omitempty"`
	CachedTokens     int64   `json:"cachedTokens,omitempty"`
	CacheWriteTokens int64   `json:"cacheWriteTokens,omitempty"`
	Cost             float64 `json:"cost,omitempty"`
	Currency         string  `json:"currency,omitempty"`
}

type UsageRequest struct {
	Credentials json.RawMessage `json:"credentials,omitempty"`
}

type UsageWindow struct {
	Name     string  `json:"name"`
	Used     float64 `json:"used"`
	Limit    float64 `json:"limit,omitempty"`
	ResetsAt int64   `json:"resetsAt,omitempty"`
	Unknown  bool    `json:"unknown,omitempty"`
	Error    string  `json:"error,omitempty"`
	AsOf     int64   `json:"asOf,omitempty"`
}

type UsageResult struct {
	Usage        Usage         `json:"usage"`
	Windows      []UsageWindow `json:"windows,omitempty"`
	AsOf         int64         `json:"asOf,omitempty"`
	ReadDeferred bool          `json:"readDeferred,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type OperationRequest struct {
	OperationID string          `json:"operationId"`
	Reference   json.RawMessage `json:"reference,omitempty"`
}

type OperationResult struct {
	OperationID string          `json:"operationId"`
	Outcome     string          `json:"outcome"`
	Result      json.RawMessage `json:"result,omitempty"`
}

type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Exposure    string          `json:"exposure,omitempty"`
}

type ToolRequest struct {
	ID        string          `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolResult struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"isError,omitempty"`
}

type AgentInspectRequest struct {
	ID       string          `json:"id"`
	Options  json.RawMessage `json:"options,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

type AgentInspectResult struct {
	Connected bool              `json:"connected"`
	Values    map[string]string `json:"values"`
	Detail    string            `json:"detail,omitempty"`
}

type AgentPlanRequest struct {
	ID       string          `json:"id"`
	Action   string          `json:"action"`
	Options  json.RawMessage `json:"options,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

type FileChange struct {
	Path       string `json:"path"`
	Body       []byte `json:"body,omitempty"`
	BeforeHash string `json:"beforeHash,omitempty"`
	Mode       uint32 `json:"mode,omitempty"`
	Remove     bool   `json:"remove,omitempty"`
}

type AgentPlan struct {
	Files []FileChange `json:"files"`
}

type SessionDocument struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	Body   []byte `json:"body"`
}

type SessionRequest struct {
	ID      string            `json:"id"`
	From    string            `json:"from"`
	To      string            `json:"to"`
	Files   []SessionDocument `json:"files"`
	Options json.RawMessage   `json:"options,omitempty"`
}

type SessionResult struct {
	Files []SessionDocument `json:"files"`
	ID    string            `json:"id,omitempty"`
}

type StateGetRequest struct {
	Key string `json:"key"`
}

type NetworkProxyRequest struct {
	URL string `json:"url"`
}

type NetworkProxyResult struct {
	URL string `json:"url,omitempty"`
}

type AccountReplaceRequest struct {
	Credentials json.RawMessage `json:"credentials"`
}

type AccountReplaceResult struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

type StateValue struct {
	Found   bool            `json:"found"`
	Version string          `json:"version,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
}

type StateCASRequest struct {
	Key     string          `json:"key"`
	Version string          `json:"version,omitempty"`
	Value   json.RawMessage `json:"value"`
}

type StateCASResult struct {
	Swapped bool   `json:"swapped"`
	Version string `json:"version"`
}
