package gateway

import (
	"context"

	"github.com/yetone/magpie/internal/extension"
)

func protocolExecutors(ctx context.Context, candidates []candidate, plan planned) ([]candidate, planned) {
	if !extension.ExecutorRequired(ctx) {
		return candidates, plan
	}
	compatible := func(candidate candidate) bool { return candidate.p.CheckExecutor(ctx) == nil }
	candidates, plan, _ = keepCandidates(candidates, plan, compatible)
	var unlisted []candidate
	var unlistedRows []Weighed
	for index, candidate := range plan.unlisted {
		if compatible(candidate) {
			unlisted = append(unlisted, candidate)
			if index < len(plan.unlistedW) {
				unlistedRows = append(unlistedRows, plan.unlistedW[index])
			}
		}
	}
	plan.unlisted, plan.unlistedW = unlisted, unlistedRows
	return candidates, plan
}
