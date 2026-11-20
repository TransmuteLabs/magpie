package dim

import (
	"strconv"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func GatewayLaunch(gateway, key, model string, contextTokens, outputTokens int) sdk.AgentLaunch {
	return sdk.AgentLaunch{Env: map[string]string{
		"DIM_BASE_URL":           gateway + "/v1",
		"DIM_API_FORMAT":         "openai-compatible",
		"DIM_API_KEY":            key,
		"DIM_MODEL":              model,
		"DIM_MAX_CONTEXT_TOKENS": strconv.Itoa(contextTokens),
		"DIM_MAX_OUTPUT_TOKENS":  strconv.Itoa(outputTokens),
	}}
}
