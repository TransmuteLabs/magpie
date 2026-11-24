package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/extension/native"
)

//go:embed magpie-plugin.json
var descriptor []byte

func init() {
	var manifest sdk.Manifest
	if err := json.Unmarshal(descriptor, &manifest); err != nil {
		panic(err)
	}
	unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, &sdk.Error{Code: "unsupported", Message: "the fixture account is provisioned", Outcome: "not-started"}
	}
	if err := native.Register(sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderUsage:   readQuota,
		sdk.MethodProviderExecute: execute,
		sdk.MethodAuthStart:       unsupported,
		sdk.MethodAuthFinish:      unsupported,
		sdk.MethodAuthRefresh:     unsupported,
		sdk.MethodAuthLogout:      unsupported,
	}}); err != nil {
		panic(err)
	}
}

func fixtureRoot(ctx context.Context) (string, error) {
	current, found := sdk.ModuleContextFrom(ctx)
	var config struct{ Root string }
	if !found || json.Unmarshal(current.Config.Values, &config) != nil || !filepath.IsAbs(config.Root) {
		return "", errors.New("missing fixture root")
	}
	return config.Root, nil
}

func readQuota(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
	root, err := fixtureRoot(ctx)
	if err != nil {
		return nil, err
	}
	var request sdk.UsageRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if call.Scope.Account != "alice" || string(call.Auth) != `{"access":"quota-fixture"}` {
		return nil, errors.New("usage did not receive the selected account")
	}
	raw, err := os.ReadFile(filepath.Join(root, "windows.json"))
	if err != nil {
		return nil, err
	}
	result := sdk.UsageResult{Revision: request.Revision}
	if err := json.Unmarshal(raw, &result.Windows); err != nil {
		return nil, err
	}
	return result, nil
}

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	root, err := fixtureRoot(ctx)
	if err != nil {
		return nil, err
	}
	var request sdk.ExecuteRequest
	decodeErr := json.Unmarshal(call.Params, &request)
	log, err := os.OpenFile(filepath.Join(root, "dispatch.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	dispatch := map[string]any{"scope": call.Scope, "model": request.Model, "protocol": request.Protocol,
		"operation": request.OperationID, "pid": os.Getpid()}
	if err := errors.Join(json.NewEncoder(log).Encode(dispatch), log.Close(), decodeErr); err != nil {
		return nil, err
	}
	if string(call.Auth) != `{"access":"quota-fixture"}` {
		return nil, errors.New("execution did not receive the selected account")
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK, SentModel: request.Model,
		Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
		return nil, err
	}
	body := fmt.Sprintf(`{"id":%q,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`,
		call.Scope.Operation, request.Model, "answered "+request.Model)
	if err := stream.Emit(ctx, "data", []byte(body)); err != nil {
		return nil, err
	}
	return sdk.ExecuteResult{Status: http.StatusOK, SentModel: request.Model, Outcome: "completed",
		Usage: &sdk.Usage{InputTokens: 13, OutputTokens: 3}}, nil
}

func main() {}
