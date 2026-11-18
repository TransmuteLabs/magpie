package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	module := authfixture.Module()
	raw, err := os.ReadFile(sdk.ManifestFile)
	var manifest sdk.Manifest
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		os.Exit(1)
	}
	module.Manifest = manifest
	if !manifest.Contract.Has(sdk.AccountAuth) {
		module.Handlers = map[string]sdk.Handler{}
	}
	action := func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
		if call.Action == "root" {
			if call.Scope.Account != "" || len(call.Auth) != 0 {
				return nil, errors.New("root action received account authority")
			}
			return map[string]bool{"root": true}, nil
		}
		if call.Action != "inspect" || call.Scope.Account != "chosen" || call.Scope.Generation == 0 ||
			string(call.Auth) != `{"token":"PRIVATE-CREDENTIAL"}` {
			return nil, errors.New("selected fixture authority was not captured")
		}
		return sdk.AccountSummary{ID: call.Scope.Account, Generation: strconv.FormatUint(call.Scope.Generation, 10)}, nil
	}
	module.Actions = map[string]sdk.Handler{"inspect": action, "root": action}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
