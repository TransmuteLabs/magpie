package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync/atomic"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type trackedInput struct {
	eof atomic.Bool
}

func (input *trackedInput) Read(buffer []byte) (int, error) {
	size, err := os.Stdin.Read(buffer)
	if errors.Is(err, io.EOF) {
		input.eof.Store(true)
	}
	return size, err
}

func main() {
	body, err := os.ReadFile(sdk.ManifestFile)
	var manifest sdk.Manifest
	if err != nil || json.Unmarshal(body, &manifest) != nil {
		os.Exit(2)
	}
	input := &trackedInput{}
	var marker atomic.Pointer[string]
	module := sdk.Module{
		Manifest: manifest,
		Actions: map[string]sdk.Handler{"mark": func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var path string
			if err := json.Unmarshal(call.Params, &path); err != nil {
				return nil, err
			}
			marker.Store(&path)
			return true, nil
		}},
		Start: func(context.Context, sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			return sdk.ModuleReady{}, func() {
				if path := marker.Load(); path != nil && !input.eof.Load() {
					_ = os.WriteFile(*path, []byte("stopped"), 0o600)
				}
			}, nil
		},
	}
	if sdk.Serve(context.Background(), input, os.Stdout, module) != nil {
		os.Exit(3)
	}
}
