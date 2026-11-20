package agent

import (
	"strings"

	"github.com/yetone/magpie/internal/extension"
	builtincline "github.com/yetone/magpie/internal/extension/builtin/cline"
)

func clineSelectModel(at place, model string, files []extension.LinkedActionFile) error {
	prefix := "cline:" + files[0].Path + ":"
	saved := peek(prefix+"entry", prefix+"lastUsedProvider", prefix+"models",
		prefix+"state", prefix+"secret", prefix+"stated")
	pick := builtincline.ModelOptions{
		Gateway: clineGateway(at), Model: model, ResolveReference: cutMagpie,
		Saved: make(map[string]string, len(saved)), Keep: make(map[string]string),
	}
	for key, value := range saved {
		pick.Saved[strings.TrimPrefix(key, prefix)] = value
	}
	if err := extension.ApplyLinkedAgentPlan(&pick, builtincline.PlanModel, files...); err != nil {
		return err
	}
	if len(pick.Keep) != 0 {
		keep := make(map[string]string, len(pick.Keep))
		for key, value := range pick.Keep {
			keep[prefix+key] = value
		}
		stash(keep)
	}
	if pick.Restoring {
		forgetValues(saved)
	}
	return nil
}
