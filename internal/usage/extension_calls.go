package usage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
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
		id, user := extensionhost.OwnCallPublicLogin(ctx)
		record := extensionCall(id, user, call)
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

func extensionCall(id, user string, call sdk.OwnCall) Record {
	record := Record{
		Provider: id, ProviderAccount: user,
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
	accounts, err := provider.ExtensionOutsideAccounts(ctx)
	if err != nil {
		return false
	}
	changed := false
	for _, account := range accounts {
		scope := account.Scope()
		id, user := account.PublicLogin()
		key := "sdk:" + id + "\x00" + scope.Account
		now := outsideIO.now()
		since, known := marks[key]
		if !known {
			since = now.Add(-outsideBack)
		}
		calls, err := account.Calls(ctx, since, now)
		if err != nil {
			continue
		}
		namespace := sha256.Sum256([]byte(scope.Plugin + "\x00" + scope.Provider + "\x00" + scope.Account))
		overlap := outsideCallOverlap{provider: id, account: user, since: since,
			prefix: "sdk:" + hex.EncodeToString(namespace[:]) + ":", pending: make(map[string]bool, len(calls))}
		records := make([]Record, 0, len(calls))
		for _, call := range calls {
			overlap.pending[call.ID] = true
			record := extensionCall(id, user, call.OwnCall)
			record.Time, record.Agent, record.Session = time.UnixMilli(call.Time).UTC(), scope.Plugin, call.Session
			record.RequestID = call.ID
			records = append(records, record)
		}
		if len(records) > 0 {
			snapshot := readLogSnapshot()
			overlap.path, overlap.info, overlap.offset = snapshot.path, snapshot.info, snapshot.off
			snapshot.visit(since, overlap.note)
		}
		if _, err := ingestReported(ctx, records, func(publish func() error) error {
			return account.Publish(ctx, publish)
		}, &overlap); err != nil {
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

type outsideCallOverlap struct {
	provider, account, prefix string
	since                     time.Time
	path                      string
	info                      os.FileInfo
	offset                    int64
	pending                   map[string]bool
}

func (overlap *outsideCallOverlap) note(record Record) {
	if overlap.pending[record.RequestID] && record.Provider == overlap.provider &&
		record.ProviderAccount == overlap.account && !record.IsReported() && !record.IsProbe() &&
		!record.Time.Before(overlap.since) {
		delete(overlap.pending, record.RequestID)
	}
}

var errOutsideLogChanged = errors.New("usage log changed during outside history import")

func (overlap *outsideCallOverlap) catchUp(ctx context.Context, path string) error {
	if overlap == nil || overlap.path == "" {
		return nil
	}
	if path != overlap.path {
		return errOutsideLogChanged
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && overlap.info == nil {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := statLogHandle(file)
	if err != nil {
		return err
	}
	unchanged := sameLogInfo(overlap.info, info)
	if unchanged && info.Size() == overlap.offset {
		return nil
	}
	if !unchanged && overlap.info != nil && (!sameLogFile(overlap.info, info) || info.Size() <= overlap.info.Size()) {
		return errOutsideLogChanged
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(file, overlap.offset, info.Size()-overlap.offset), 64<<10)
	for len(overlap.pending) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		overlap.offset += int64(len(line))
		var record Record
		if json.Unmarshal(line, &record) == nil {
			overlap.note(record)
		}
	}
	overlap.info = info
	return nil
}
