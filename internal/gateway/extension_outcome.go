package gateway

import (
	"net/http"

	"github.com/yetone/magpie/internal/provider"
)

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
