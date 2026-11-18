package gateway

import (
	"errors"
	"fmt"
	"net/http"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type decisionOperationError struct {
	cause     error
	module    string
	operation string
}

func (failure *decisionOperationError) Error() string {
	return failure.cause.Error() + "; automatic replay stopped; inspect the operation before retrying"
}

func (failure *decisionOperationError) Unwrap() error { return failure.cause }

func terminalExtensionError(err error) bool {
	var outcome *extensionhost.OutcomeError
	var rpc *sdk.Error
	return errors.As(err, &outcome) && terminalExtensionOutcome(outcome.Outcome) ||
		errors.As(err, &rpc) && terminalExtensionOutcome(rpc.Outcome)
}

func terminalExtensionOutcome(outcome string) bool {
	return outcome == "accepted" || outcome == "uncertain" || outcome == "completed"
}

func decisionFailure(err error, attempt *provider.ExtensionAttempt) error {
	if err == nil {
		return nil
	}
	var failure *decisionOperationError
	if errors.As(err, &failure) {
		return err
	}
	outcome := attempt.Outcome(true)
	if !terminalExtensionError(err) && !terminalExtensionError(outcome) {
		return err
	}
	failure = &decisionOperationError{cause: errors.Join(err, outcome)}
	if attempt != nil {
		failure.module, failure.operation = attempt.Operation()
	}
	return failure
}

func decisionFailureHeaders(header http.Header, err error) {
	header.Set("X-Should-Retry", "false")
	var failure *decisionOperationError
	if errors.As(err, &failure) && failure.operation != "" {
		header.Set("X-Magpie-Operation", failure.operation)
		header.Set("X-Magpie-Extension", failure.module)
	}
}

func sharedClassifierError(err error) error {
	var failure *decisionOperationError
	if errors.As(err, &failure) {
		return &decisionOperationError{cause: failure.cause}
	}
	return err
}

func extensionSideOutcome(writer http.ResponseWriter, attempt *provider.ExtensionAttempt, failed bool) error {
	outcome := attempt.Outcome(failed)
	if outcome == nil {
		return nil
	}
	module, operation := attempt.Operation()
	writer.Header().Set("X-Should-Retry", "false")
	if operation != "" {
		writer.Header().Set("X-Magpie-Operation", operation)
		writer.Header().Set("X-Magpie-Extension", module)
	}
	return fmt.Errorf("%w; automatic replay stopped; inspect the operation before retrying", outcome)
}

func extensionOperationFailure(writer http.ResponseWriter, alive *keptAlive, protocol provider.Protocol, model string, streaming bool, outcome error, attempt *provider.ExtensionAttempt) (int, string) {
	message := outcome.Error() + "; automatic replay stopped; inspect the operation before retrying"
	module, operation := attempt.Operation()
	if operation != "" {
		writer.Header().Set("X-Magpie-Operation", operation)
		writer.Header().Set("X-Magpie-Extension", module)
		message += " (" + operation + ")"
	}
	writer.Header().Set("X-Should-Retry", "false")
	if streaming {
		streamError(writer, protocol, model, http.StatusConflict, message)
	} else {
		failTo(writer, alive, protocol, http.StatusConflict, message)
	}
	return http.StatusConflict, message
}
