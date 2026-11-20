package agy

import sdk "github.com/yetone/magpie/pkg/extension"

func GatewayLaunch() sdk.AgentLaunch {
	return sdk.AgentLaunch{
		Env:   map[string]string{"GEMINI_API_KEY": "{key}", "GOOGLE_GEMINI_BASE_URL": "{url}"},
		Args:  map[string]string{"--settings": "{config}"},
		Model: "--model",
		Plain: []string{"remote-control", "--remote-control", "-remote-control"},
	}
}
