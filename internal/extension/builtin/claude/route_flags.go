package claude

import (
	"bytes"
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type RouteFlag string

const (
	ExploreFlag RouteFlag = "CLAUDE_CODE_DISABLE_EXPLORE_INHERIT_CAP"
	ReviewFlag  RouteFlag = "CLAUDE_CODE_AUTO_MODE_SERVER"
	SearchFlag  RouteFlag = "ENABLE_TOOL_SEARCH"
)

type RouteFlagsOptions struct {
	Routed         bool
	SuppressReview bool
	Desktop        bool
	Previous       map[RouteFlag]string
}

func PlanRouteFlags(options RouteFlagsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, map[RouteFlag]string, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, nil, errors.New("Claude route flags action requires a settings.json snapshot")
	}
	type flagPolicy struct {
		flag    RouteFlag
		value   string
		install bool
	}
	policies := []flagPolicy{
		{ReviewFlag, "0", options.Routed && options.SuppressReview},
		{SearchFlag, "true", options.Routed},
		{ExploreFlag, "1", options.Desktop},
	}
	if !options.Routed {
		policies[0], policies[1], policies[2] = policies[2], policies[0], policies[1]
	}

	file := settings.Files[0]
	body := file.Body
	plain := jsonc.ToJSON(edit.TrimBOM(body))
	next := make(map[RouteFlag]string, len(policies))
	for _, policy := range policies {
		key := "env." + string(policy.flag)
		current := gjson.GetBytes(plain, key)
		value := current.String()
		owned := value != "" && value == options.Previous[policy.flag] &&
			(policy.flag != ExploreFlag || value == policy.value)
		var err error
		switch {
		case !policy.install:
			if owned {
				body, err = edit.PatchJSON(body, nil, []string{key})
			}
		case owned && value == policy.value:
			next[policy.flag] = value
		case !owned && (value != "" || current.Exists() && policy.flag != ReviewFlag):
		default:
			body, err = edit.PatchJSON(body, []edit.KV{{Path: key, Value: policy.value}}, nil)
			next[policy.flag] = policy.value
		}
		if err != nil {
			return sdk.AgentPlan{}, nil, edit.AtFile(file.Path, err)
		}
	}
	if bytes.Equal(file.Body, body) {
		return sdk.AgentPlan{}, next, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}}, next, nil
}
