package provider

import (
	"context"
	"errors"
	"sync"

	extensionhost "github.com/yetone/magpie/internal/extension"
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
}

func WithExtensionAttempt(ctx context.Context) (context.Context, *ExtensionAttempt) {
	attempt := &ExtensionAttempt{}
	return context.WithValue(ctx, extensionAttemptKey{}, attempt), attempt
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
	if attempt.usage == nil {
		return nil
	}
	usage := *attempt.usage
	return &usage
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
		attempt.mu.Unlock()
	}
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

func extensionUsage(ctx context.Context, usage *sdk.Usage) {
	if attempt := extensionAttemptOf(ctx); attempt != nil && usage != nil {
		attempt.mu.Lock()
		copy := *usage
		attempt.usage = &copy
		attempt.mu.Unlock()
	}
}
