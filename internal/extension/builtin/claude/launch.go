package claude

import sdk "github.com/yetone/magpie/pkg/extension"

func GatewayLaunch() sdk.AgentLaunch {
	return sdk.AgentLaunch{
		Args: map[string]string{"--settings": "{config}"},
	}
}
