package extension

import (
	"context"
	"net/http"
	"time"
)

// LinkedGatewayFeature specializes a feature with the host's caller and
// ledger types. These registrations stay inside the linked process.
type LinkedGatewayFeature[Caller, Record any] struct {
	FeatureContribution
	Budget   LinkedBudgetLimiter[Caller]
	Observer func() LinkedUsageObserver[Record]
}

// LinkedBudgetRefusal describes a refusal as reported to the client.
type LinkedBudgetRefusal interface {
	error
	// RetryAfter returns the reset delay in whole seconds.
	RetryAfter(now time.Time) int
	// ResetAt returns the reset time.
	ResetAt() time.Time
	// KeyWide reports whether every model on the key is held.
	KeyWide() bool
}

// LinkedBudgetLimiter holds a reservation until release, or refuses the
// request. The host supplies its authenticated caller and admission time.
// Body size is in bytes, or -1 when unknown.
type LinkedBudgetLimiter[Caller any] interface {
	Reserve(who Caller, body int64, model string, now time.Time) (release func(), refused LinkedBudgetRefusal)
}

// LinkedBudgetSpender optionally identifies exhausted model scopes.
type LinkedBudgetSpender[Caller, Scope any] interface {
	Spent(who Caller, now time.Time) []Scope
}

// LinkedUsageObserver observes the host's gateway and ledger. The host
// starts it while serving and owns the capture and publication policy.
type LinkedUsageObserver[Record any] interface {
	// Serve runs while this process serves the gateway.
	Serve() (stop func())
	Bodies() LinkedUsageBodies
	// Begin may return nil to leave a request unwatched.
	Begin(LinkedUsageRequest) LinkedUsageWatch[Record]
	// Ledger receives each published row, including rows outside a watch.
	Ledger(Record)
}

type LinkedUsageWatch[Record any] interface {
	// Attempt includes unbilled failures and retries, timed per attempt.
	Attempt(Record)
	End(Record)
}

type LinkedUsageRequest struct {
	Header http.Header
	// Session is the native agent session when available, else magpie's.
	Agent, Session string
	Kind           string
	// Plain is a local request without a gateway key or relay.
	Plain bool
}

type LinkedUsageBodies int

const (
	LinkedNoBodies       LinkedUsageBodies = iota
	LinkedCapturedBodies                   // the first 256 KiB retained for recent calls
	LinkedWholeBodies                      // the whole reply spooled while it streams
)

// LinkedMiddlewareInfo describes the request in the agent's own API.
type LinkedMiddlewareInfo struct {
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Path     string `json:"path"`
	Agent    string `json:"agent,omitempty"`
}

// LinkedMiddlewareReject ends a request in the agent API's error shape.
type LinkedMiddlewareReject struct {
	Status  int
	Message string
}

// LinkedAgentMiddleware is the in-process contract for trusted Go features.
type LinkedAgentMiddleware interface {
	// Begin returns nil to leave the request alone.
	Begin(LinkedMiddlewareInfo) LinkedMiddlewareRun
}

// LinkedMiddlewareRun handles one request. The owner calls End after the
// wrapped response's completion callback.
type LinkedMiddlewareRun interface {
	Request(body []byte) ([]byte, *LinkedMiddlewareReject)
	Wrap(http.ResponseWriter) (http.ResponseWriter, func())
	End()
}

type LinkedMiddlewareTarget struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	Model   string `json:"model"`
}

// LinkedMiddlewareTried optionally transforms one attempt in the agent's API.
type LinkedMiddlewareTried interface {
	Tried(body []byte, to LinkedMiddlewareTarget) []byte
}

// LinkedMiddlewareAccount describes a subscription inside the linked process.
// A bridge projects an opaque reference before exposing the account to
// extension code outside this trust boundary. An empty Agent denotes a key.
type LinkedMiddlewareAccount struct {
	Agent, User, Plan string
	ID, Name, Model   string
}

// LinkedMiddlewareAccounts can skip an account without resting it.
type LinkedMiddlewareAccounts interface {
	Account(LinkedMiddlewareAccount) (skip bool, by string)
}

// LinkedMasker returns masked values and their replacement count. It restores
// values in plain or escaped text and wraps streamed replies for restoration.
// The host decides when masking is required and whether a call may leave.
type LinkedMasker interface {
	JSON(body []byte) ([]byte, int)
	Text(s string) (string, int)
	Restore(s string, escaped bool) string
	Writer(http.ResponseWriter) (http.ResponseWriter, func())
}

// LinkedMaskHit gives a match's kind, finder and placeholder. Head and Len
// carry a permitted prefix and length. Action is empty, "warn" or "block".
type LinkedMaskHit struct {
	Kind, By, Placeholder, Action, Head string
	Len                                 int
}

// LinkedMaskTeller additionally reports one hit per occurrence and can look
// up a remembered value for the owner's authorized diagnostics.
type LinkedMaskTeller interface {
	JSONHits(body []byte) ([]byte, []LinkedMaskHit)
	TextHits(s string) (string, []LinkedMaskHit)
	Value(placeholder string) (string, bool)
}

// LinkedMaskContextTeller can wait on local inference and honors cancellation.
type LinkedMaskContextTeller interface {
	JSONHitsContext(ctx context.Context, body []byte) ([]byte, []LinkedMaskHit)
	TextHitsContext(ctx context.Context, s string) (string, []LinkedMaskHit)
}

// LinkedGatewayTool identifies a built-in tool service with a gateway owner.
type LinkedGatewayTool string

const (
	LinkedWebSearch   LinkedGatewayTool = "web_search"
	LinkedPluginTools LinkedGatewayTool = "plugin_tools"
)
