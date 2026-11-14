package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/yetone/magpie/internal/access"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type ownCallAppenderKey struct{}

func WithOwnCallAppender(ctx context.Context, appendRecord func(Record)) context.Context {
	return context.WithValue(ctx, ownCallAppenderKey{}, appendRecord)
}

func init() {
	extensionhost.OnOwnCallContext(func(ctx context.Context, scope sdk.Scope, call sdk.OwnCall) {
		record := extensionCall(scope, call)
		record.Agent, record.Session = "magpie", scope.Session
		who := access.Caller(ctx)
		record.CallerKeyID, record.CallerKeyName, record.CallerIssuerID = who.KeyID, who.KeyName, who.IssuerID()
		record.Operation = scope.Operation
		appendRecord := Append
		if appender, found := ctx.Value(ownCallAppenderKey{}).(func(Record)); found && appender != nil {
			appendRecord = appender
		}
		appendRecord(record)
	})
}

func extensionCall(scope sdk.Scope, call sdk.OwnCall) Record {
	record := Record{
		Provider: provider.ExtensionProviderID(scope.Plugin, scope.Provider), ProviderAccount: scope.Account,
		Model: call.Model, Requested: call.Model, Kind: call.Kind, Status: call.Status, Millis: call.Millis,
		Error: clipProbeError(call.Error),
	}
	totals := call.Usage
	record.Input, record.Output = int(totals.InputTokens), int(totals.OutputTokens)
	record.CacheRead, record.CacheWrite = int(totals.CachedTokens), int(totals.CacheWriteTokens)
	if totals.Cost != 0 || totals.Currency != "" {
		record.ReportedCost = &ReportedCost{Amount: totals.Cost, Currency: totals.Currency}
	}
	if facts := totals.Facts; facts != nil {
		extensionCallCounts(&record, facts.ExecutionCounts)
		if facts.Deferred != nil {
			var deferred Record
			extensionCallCounts(&deferred, *facts.Deferred)
			record.Input += deferred.Input
			record.Output += deferred.Output
			record.CacheRead += deferred.CacheRead
			record.CacheWrite += deferred.CacheWrite
			record.CacheWrite1h += deferred.CacheWrite1h
			record.Reasoning += deferred.Reasoning
		}
		if facts.CacheUnknown != nil {
			record.CacheUnknown = *facts.CacheUnknown
		}
		for _, field := range []struct {
			from *string
			to   *string
		}{
			{facts.ServedModel, &record.Served}, {facts.UpstreamProvider, &record.Upstream},
			{facts.RequestID, &record.RequestID}, {facts.ResponseID, &record.UpstreamResponseID},
			{facts.Stop, &record.Stop}, {facts.Tier, &record.TierServed}, {facts.ErrorType, &record.ErrType},
		} {
			if field.from != nil {
				*field.to = *field.from
			}
		}
		if facts.Credits != nil {
			credits := *facts.Credits
			record.Credits = &credits
		}
		if facts.Cost != nil {
			record.ReportedCost = &ReportedCost{Amount: facts.Cost.Amount, Currency: facts.Cost.Currency}
		}
	}
	record.CacheUnknown = record.CacheUnknown && record.CacheRead == 0 && record.CacheWrite == 0
	record.OwnCost = record.ReportedCost.USD()
	return record
}

func extensionCallCounts(record *Record, counts sdk.ExecutionCounts) {
	for _, field := range []struct {
		from *int64
		to   *int
	}{
		{counts.InputTokens, &record.Input}, {counts.OutputTokens, &record.Output},
		{counts.CachedTokens, &record.CacheRead}, {counts.CacheWriteTokens, &record.CacheWrite},
		{counts.CacheWrite1hTokens, &record.CacheWrite1h}, {counts.ReasoningTokens, &record.Reasoning},
	} {
		if field.from != nil {
			*field.to = int(*field.from)
		}
	}
}

func readExtensionOutsideCalls(ctx context.Context, marks map[string]time.Time) bool {
	accounts, err := provider.ExtensionOutsideAccounts()
	if err != nil {
		return false
	}
	changed := false
	for _, account := range accounts {
		scope := account.Scope()
		key := "sdk:" + account.ProviderID() + "\x00" + scope.Account
		now := outsideIO.now()
		since, known := marks[key]
		if !known {
			since = now.Add(-outsideBack)
		}
		calls, err := account.Calls(ctx, since, now)
		if err != nil {
			continue
		}
		through := map[string]bool{}
		Visit(since, func(record Record) {
			if record.Provider == account.ProviderID() && record.ProviderAccount == scope.Account &&
				!record.IsReported() && record.RequestID != "" {
				through[record.RequestID] = true
			}
		})
		namespace := sha256.Sum256([]byte(scope.Plugin + "\x00" + scope.Provider + "\x00" + scope.Account))
		records := make([]Record, 0, len(calls))
		for _, call := range calls {
			if through[call.ID] {
				continue
			}
			record := extensionCall(scope, call.OwnCall)
			record.Time, record.Agent, record.Session = time.UnixMilli(call.Time).UTC(), scope.Plugin, call.Session
			record.RequestID = "sdk:" + hex.EncodeToString(namespace[:]) + ":" + call.ID
			records = append(records, record)
		}
		if account.Current(ctx) != nil {
			continue
		}
		if _, err := IngestReported(records); err != nil {
			continue
		}
		next := now.Add(-outsideOverlap)
		if next.Before(since) {
			next = since
		}
		marks[key] = next
		changed = true
	}
	return changed
}
