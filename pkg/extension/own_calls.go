package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

const (
	MethodUsageAppend   = "usage.append"
	MethodProviderCalls = "provider.calls"
	MaxOutsideCalls     = 500
	MaxOutsideCallPages = 32
)

type OwnCall struct {
	Kind   string `json:"kind,omitempty"`
	Model  string `json:"model"`
	Status int    `json:"status,omitempty"`
	Millis int64  `json:"millis,omitempty"`
	Error  string `json:"error,omitempty"`
	Usage  Usage  `json:"usage"`
}

type OwnCallRequest struct {
	Call OwnCall `json:"call"`
}

type OwnCallResult struct {
	Accepted bool `json:"accepted"`
}

type OutsideCall struct {
	OwnCall
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Session string `json:"session,omitempty"`
}

type OutsideCallsRequest struct {
	Since  int64  `json:"since"`
	Until  int64  `json:"until"`
	Cursor string `json:"cursor,omitempty"`
}

type OutsideCallsResult struct {
	Calls      []OutsideCall `json:"calls"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

func IsOwnCallHostMethod(method string) bool {
	return method == MethodUsageAppend
}

func ValidateOwnCall(manifest Manifest, call OwnCall) error {
	if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(UsageReporter) ||
		!ownCallKind(call.Kind) || !validCallFacts(manifest, call) {
		return errors.New("own calls require SDK API 1.8 and bounded usage facts")
	}
	return nil
}

func ownCallKind(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '_' && character != '-' {
					return false
				}
			}
		}
	}
	return true
}

func validCallFacts(manifest Manifest, call OwnCall) bool {
	if !(call.Model != "" && validExecutionText(call.Model, 300) &&
		!strings.ContainsFunc(call.Model, unicode.IsSpace) &&
		(call.Status == 0 || call.Status >= 100 && call.Status <= 599) &&
		call.Millis >= 0 && call.Millis <= 24*60*60*1000 && validExecutionText(call.Error, 1024) &&
		ValidateExecutionUsage(manifest, &call.Usage) == nil) {
		return false
	}
	facts := call.Usage.Facts
	if facts == nil || facts.Deferred == nil {
		return true
	}
	maximum := min(int64(^uint(0)>>1), int64(1<<53-1))
	for _, pair := range [][2]*int64{
		{facts.InputTokens, facts.Deferred.InputTokens},
		{facts.OutputTokens, facts.Deferred.OutputTokens},
		{facts.CachedTokens, facts.Deferred.CachedTokens},
		{facts.CacheWriteTokens, facts.Deferred.CacheWriteTokens},
		{facts.CacheWrite1hTokens, facts.Deferred.CacheWrite1hTokens},
		{facts.ReasoningTokens, facts.Deferred.ReasoningTokens},
	} {
		if pair[0] != nil && pair[1] != nil && *pair[1] > maximum-*pair[0] {
			return false
		}
	}
	return true
}

func DecodeOutsideCallsRequest(manifest Manifest, scope Scope, raw json.RawMessage) (OutsideCallsRequest, error) {
	var request OutsideCallsRequest
	declared := false
	for _, provider := range manifest.Providers {
		declared = declared || provider.ID == scope.Provider && provider.Calls
	}
	if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(UsageReporter) ||
		!manifest.Contract.Has(AccountAuth) || !declared || scope.Plugin != manifest.ID ||
		scope.Account == "" || scope.Generation == 0 || scope.Session != "" || scope.Operation != "" || scope.Attempt != 0 ||
		len(raw) > MaxHostBytes || decodeParams(raw, &request) != nil ||
		request.Since < 946684800000 || request.Until < request.Since || request.Until > 1<<53-1 ||
		!validExecutionText(request.Cursor, 1024) {
		return request, errors.New("outside calls require SDK API 1.8 and the selected account read window")
	}
	return request, nil
}

func DecodeOutsideCallsResult(manifest Manifest, request OutsideCallsRequest, raw json.RawMessage) (OutsideCallsResult, error) {
	var result OutsideCallsResult
	if len(raw) > MaxHostBytes || decodeParams(raw, &result) != nil || result.Calls == nil ||
		len(result.Calls) > MaxOutsideCalls || !validExecutionText(result.NextCursor, 1024) {
		return result, errors.New("invalid bounded outside-call page")
	}
	for _, call := range result.Calls {
		if call.ID == "" || !validExecutionText(call.ID, 300) ||
			!validExecutionText(call.Session, 300) || call.Time < request.Since || call.Time > request.Until ||
			call.Kind != "" && !ownCallKind(call.Kind) || !validCallFacts(manifest, call.OwnCall) {
			return result, errors.New("invalid outside call or call outside the requested window")
		}
	}
	return result, nil
}

func (stream *Stream) AppendOwnCall(ctx context.Context, call OwnCall) (OwnCallResult, error) {
	var result OwnCallResult
	err := stream.HostCall(ctx, MethodUsageAppend, OwnCallRequest{Call: call}, &result)
	return result, err
}
