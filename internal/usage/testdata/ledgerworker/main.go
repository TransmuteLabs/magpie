package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		current, found := sdk.ModuleContextFrom(ctx)
		var config struct {
			Sink string `json:"sink"`
		}
		if !found || json.Unmarshal(current.Config.Values, &config) != nil || config.Sink == "" {
			return nil, fmt.Errorf("missing fixture observation sink")
		}
		if call.Method == sdk.MethodRequestObserve {
			if call.Auth != nil || call.Scope != (sdk.Scope{Plugin: manifest.ID}) {
				return nil, fmt.Errorf("request observation exposed selected credentials or scope")
			}
			if stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "fixture"}, nil) == nil {
				return nil, fmt.Errorf("request observation acquired host service authority")
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Sink, bytes.NewReader(call.Params))
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		return struct{}{}, err
	}
	handlers := make(map[string]sdk.Handler)
	for _, method := range sdk.RequiredMethods(manifest) {
		handlers[method] = handler
	}
	module := sdk.Module{Manifest: manifest, Handlers: handlers}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
