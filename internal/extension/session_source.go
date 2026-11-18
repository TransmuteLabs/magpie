package extension

import (
	"encoding/json"
	"errors"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func validateSessionSourceScope(manifest sdk.Manifest, method string, scope sdk.Scope, params any) error {
	if method != sdk.MethodSessionSourceRead {
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	request, err := sdk.DecodeSessionSourceRequest(manifest, raw)
	if err != nil {
		return err
	}
	if scope.Session != request.Locator.SessionID || scope.Provider != "" || scope.Account != "" || scope.Generation != 0 {
		return errors.New("session source read requires its local selected session scope")
	}
	return nil
}
