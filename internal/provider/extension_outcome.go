package provider

import (
	"context"
	"errors"
	"sync"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionAttemptKey struct{}

type ExtensionAttempt struct {
	mu        sync.Mutex
	accepted  bool
	completed bool
	err       error
	usage     *sdk.Usage
	scope     sdk.Scope
	refusal   *requestpolicy.Refusal

	projectRefusal *sdk.Error
}

func WithExtensionAttempt(ctx context.Context) (context.Context, *ExtensionAttempt) {
	attempt := &ExtensionAttempt{}
	ctx = extensionPolicyContext(ctx)
	return context.WithValue(ctx, extensionAttemptKey{}, attempt), attempt
}

// ExtensionOutcome keeps lower-level request helpers on the same completion
// boundary as their caller, before a protocol or account fallback can run.
func ExtensionOutcome(ctx context.Context, failed bool) error {
	return extensionAttemptOf(ctx).Outcome(failed)
}

func (attempt *ExtensionAttempt) Outcome(failed bool) error {
	if attempt == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.err != nil {
		return attempt.err
	}
	if failed && attempt.accepted {
		return &extensionhost.OutcomeError{Outcome: "accepted"}
	}
	if failed && attempt.completed {
		return &extensionhost.OutcomeError{Outcome: "completed"}
	}
	return nil
}

func (attempt *ExtensionAttempt) Usage() *sdk.Usage {
	if attempt == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.usage.Clone()
}

func (attempt *ExtensionAttempt) UsageFor(p Provider) *sdk.Usage {
	if attempt == nil {
		return nil
	}
	scope, ok := p.ExtensionScope()
	if !ok {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.usage == nil || attempt.scope.Plugin != scope.Plugin || attempt.scope.Provider != scope.Provider ||
		attempt.scope.Account != scope.Account || attempt.scope.Generation != scope.Generation {
		return nil
	}
	return attempt.usage.Clone()
}

func (attempt *ExtensionAttempt) Operation() (string, string) {
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.scope.Plugin, attempt.scope.Operation
}

func extensionBegin(ctx context.Context, scope sdk.Scope) {
	if attempt := extensionAttemptOf(ctx); attempt != nil {
		attempt.mu.Lock()
		attempt.scope = scope
		attempt.usage = nil
		attempt.refusal = nil
		attempt.projectRefusal = nil
		attempt.mu.Unlock()
	}
}

func extensionProjectRefusal(ctx context.Context, scope sdk.Scope, err error) {
	rejected, ok := err.(*sdk.Error)
	if !ok || rejected == nil || rejected.Outcome != "not-started" || scope.Plugin != "opencode" {
		return
	}
	if rejected.Code != "project_required" && rejected.Code != "invalid_project" {
		return
	}
	switch scope.Provider {
	case "opencode-go", "opencode-zen", "opencode-zen-free":
	default:
		return
	}
	if attempt := extensionAttemptOf(ctx); attempt != nil {
		attempt.mu.Lock()
		if attempt.scope == scope && attempt.err == nil && !attempt.accepted && !attempt.completed {
			attempt.projectRefusal = rejected
		}
		attempt.mu.Unlock()
	}
}

func (attempt *ExtensionAttempt) TakeProjectRefusal(scope sdk.Scope) *sdk.Error {
	if attempt == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.scope.Plugin != scope.Plugin || attempt.scope.Provider != scope.Provider || attempt.scope.Account != scope.Account {
		return nil
	}
	refusal := attempt.projectRefusal
	attempt.projectRefusal = nil
	return refusal
}

func extensionAttemptOf(ctx context.Context) *ExtensionAttempt {
	attempt, _ := ctx.Value(extensionAttemptKey{}).(*ExtensionAttempt)
	return attempt
}

func extensionAccepted(ctx context.Context) {
	if attempt := extensionAttemptOf(ctx); attempt != nil {
		attempt.mu.Lock()
		attempt.accepted = true
		attempt.mu.Unlock()
	}
}

func extensionCompleted(ctx context.Context) {
	if attempt := extensionAttemptOf(ctx); attempt != nil {
		attempt.mu.Lock()
		attempt.completed = true
		attempt.mu.Unlock()
	}
}

func extensionOutcome(ctx context.Context, err error) {
	var outcome *extensionhost.OutcomeError
	var rpc *sdk.Error
	terminal := errors.As(err, &outcome) && (outcome.Outcome == "uncertain" || outcome.Outcome == "accepted" || outcome.Outcome == "completed") ||
		errors.As(err, &rpc) && (rpc.Outcome == "uncertain" || rpc.Outcome == "accepted" || rpc.Outcome == "completed")
	if attempt := extensionAttemptOf(ctx); attempt != nil && terminal {
		attempt.mu.Lock()
		attempt.err = err
		attempt.mu.Unlock()
	}
}

func extensionSelectionFailure(ctx context.Context, scope sdk.Scope, err error) {
	var revoked *sdk.Error
	if !errors.As(err, &revoked) || revoked.Code != "stale_generation" && revoked.Code != "not_found" {
		return
	}
	Changed()
	if attempt := extensionAttemptOf(ctx); attempt != nil {
		attempt.mu.Lock()
		if attempt.err == nil && !attempt.accepted && !attempt.completed {
			attempt.err = &sdk.Error{Code: revoked.Code, Message: revoked.Message, Outcome: "not-started"}
			attempt.scope = scope
		}
		attempt.mu.Unlock()
	}
}

func extensionUsage(ctx context.Context, usage *sdk.Usage) {
	if attempt := extensionAttemptOf(ctx); attempt != nil && usage != nil {
		attempt.mu.Lock()
		attempt.usage = usage.Clone()
		attempt.mu.Unlock()
	}
}

// MergeExtensionOutcome joins a finished child execution's replay fence into
// its request. Usage stays with the child for the caller to normalize and sum.
func MergeExtensionOutcome(ctx context.Context, child *ExtensionAttempt, failed bool) {
	parent := extensionAttemptOf(ctx)
	if parent == nil || child == nil || parent == child {
		return
	}
	outcome := child.Outcome(failed)
	child.mu.Lock()
	scope, accepted, completed := child.scope, child.accepted, child.completed
	child.mu.Unlock()
	parent.mu.Lock()
	defer parent.mu.Unlock()
	if parent.err == nil {
		if outcome != nil || accepted || completed {
			parent.scope = scope
		}
		parent.err = outcome
	}
	parent.accepted = parent.accepted || accepted
	parent.completed = parent.completed || completed
}
