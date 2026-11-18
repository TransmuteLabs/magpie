package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	handler := func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		if call.Auth != nil || call.Scope != (sdk.Scope{Plugin: manifest.ID}) {
			return nil, errors.New("unexpected budget scope")
		}
		if stream.HostCall(ctx, sdk.MethodStateCAS, nil, nil) == nil {
			return nil, errors.New("budget policy acquired callback authority")
		}
		config, ok := sdk.ModuleContextFrom(ctx)
		var options struct {
			Sink string `json:"sink"`
		}
		if !ok || json.Unmarshal(config.Config.Values, &options) != nil || options.Sink == "" {
			return nil, errors.New("missing budget fixture sink")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, options.Sink+"/"+call.Method, bytes.NewReader(call.Params))
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, &sdk.Error{Code: "unavailable", Message: "synthetic private worker failure"}
		}
		var result json.RawMessage
		err = json.NewDecoder(response.Body).Decode(&result)
		return result, err
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Module{
		Manifest: manifest, Handlers: map[string]sdk.Handler{
			sdk.MethodBudgetReserve: handler, sdk.MethodBudgetSpent: handler,
		},
	}); err != nil {
		panic(err)
	}
}
