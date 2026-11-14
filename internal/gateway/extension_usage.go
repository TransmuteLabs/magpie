package gateway

import (
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func normalizedExtensionUsage(selected provider.Provider, attempt *provider.ExtensionAttempt, current Usage) Usage {
	totals := attempt.UsageFor(selected)
	return normalizedExtensionTotals(totals, current)
}

func normalizedExtensionTotals(totals *sdk.Usage, current Usage) Usage {
	if totals == nil {
		return current
	}
	if facts := totals.Facts; facts != nil {
		current = normalizedExtensionCounts(facts.ExecutionCounts, current)
		if facts.CacheUnknown != nil {
			current.cacheUnknown = *facts.CacheUnknown
		}
		for _, field := range []struct {
			from *string
			to   *string
		}{
			{facts.ServedModel, &current.Served}, {facts.UpstreamProvider, &current.Upstream},
			{facts.RequestID, &current.RequestID}, {facts.ResponseID, &current.UpstreamResponseID},
			{facts.Stop, &current.Stop}, {facts.Tier, &current.Tier}, {facts.ErrorType, &current.ErrType},
		} {
			if field.from != nil {
				*field.to = *field.from
			}
		}
		if facts.Deferred != nil {
			deferred := Usage{}
			if current.Deferred != nil {
				deferred = *current.Deferred
			}
			deferred = normalizedExtensionCounts(*facts.Deferred, deferred)
			current.Deferred = &deferred
		}
		if facts.Credits != nil {
			credits := *facts.Credits
			current.Credits = &credits
		}
		if facts.Cost != nil {
			current.ReportedCost = &usage.ReportedCost{Amount: facts.Cost.Amount, Currency: facts.Cost.Currency}
		}
		return current
	}
	if current.CacheWrite != int(totals.CacheWriteTokens) {
		current.CacheWrite1h = 0
		current.cacheCreationReported, current.cacheCreationHasCounts = false, false
	}
	current.Input, current.Output = int(totals.InputTokens), int(totals.OutputTokens)
	current.CacheRead, current.CacheWrite = int(totals.CachedTokens), int(totals.CacheWriteTokens)
	current.cacheUnknown = false
	if totals.Cost != 0 || totals.Currency != "" {
		current.ReportedCost = &usage.ReportedCost{Amount: totals.Cost, Currency: totals.Currency}
	}
	return current
}

func normalizedExtensionCounts(counts sdk.ExecutionCounts, current Usage) Usage {
	if counts.CacheWriteTokens != nil && *counts.CacheWriteTokens != int64(current.CacheWrite) && counts.CacheWrite1hTokens == nil {
		current.CacheWrite1h = 0
		current.cacheCreationReported, current.cacheCreationHasCounts = false, false
	}
	for _, field := range []struct {
		from *int64
		to   *int
	}{
		{counts.InputTokens, &current.Input}, {counts.OutputTokens, &current.Output},
		{counts.CachedTokens, &current.CacheRead}, {counts.CacheWriteTokens, &current.CacheWrite},
		{counts.CacheWrite1hTokens, &current.CacheWrite1h}, {counts.ReasoningTokens, &current.Reasoning},
	} {
		if field.from != nil {
			*field.to = int(*field.from)
		}
	}
	current.CacheWrite1h = min(current.CacheWrite1h, current.CacheWrite)
	if counts.CacheWrite1hTokens != nil {
		current.cacheCreationReported, current.cacheCreationHasCounts = true, true
	}
	if counts.CachedTokens != nil || counts.CacheWriteTokens != nil {
		current.cacheUnknown = false
	}
	return current
}

func accountedUsage(record usage.Record, totals Usage) usage.Record {
	billed := totals.billed()
	record.Input, record.Output = billed.Input, billed.Output
	record.CacheRead, record.CacheWrite, record.CacheWrite1h = billed.CacheRead, billed.CacheWrite, billed.CacheWrite1h
	record.CacheUnknown, record.Reasoning = billed.cacheNotCounted(), billed.Reasoning
	record.Served, record.Upstream, record.TierServed = totals.Served, totals.Upstream, totals.Tier
	record.Credits, record.ReportedCost = totals.Credits, totals.ReportedCost
	record.OwnCost = totals.ReportedCost.USD()
	record.RequestID, record.Stop = totals.RequestID, totals.Stop
	record.ResponseID = ""
	if !record.HandedOn {
		record.ResponseID = totals.ResponseID
	}
	record.UpstreamResponseID = totals.UpstreamResponseID
	failedWith(&record, record.Status, record.Error, totals.ErrType)
	return record
}
