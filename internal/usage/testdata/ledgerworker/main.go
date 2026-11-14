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
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodLedgerObserve: func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			current, found := sdk.ModuleContextFrom(ctx)
			var config struct {
				Sink string `json:"sink"`
			}
			if !found || json.Unmarshal(current.Config.Values, &config) != nil || config.Sink == "" {
				return nil, fmt.Errorf("missing fixture ledger sink")
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
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
