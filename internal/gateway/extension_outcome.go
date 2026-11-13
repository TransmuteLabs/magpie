package gateway

import (
	"fmt"
	"net/http"

	"github.com/yetone/magpie/internal/provider"
)

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
