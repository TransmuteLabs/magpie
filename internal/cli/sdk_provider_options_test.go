package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type providerOptionsCLIHost struct {
	sdkCLIHost
	closed, writes int
}

func (host *providerOptionsCLIHost) Close(context.Context) error {
	host.closed++
	return nil
}

func (host *providerOptionsCLIHost) ProviderOptions(context.Context, string, string) (*extension.ProviderOptionsSnapshot, error) {
	return &extension.ProviderOptionsSnapshot{Module: "fixture", Provider: "vendor", PackageRevision: "package", ConfigVersion: "3"}, nil
}

func (host *providerOptionsCLIHost) CompareAndSwapProviderOptions(_ context.Context, module, provider, revision, expected string,
	values map[string]bool, confirmed []string) (sdk.ConfigSnapshot, error) {
	if module != "fixture" || provider != "vendor" || revision != "package" || expected != "3" ||
		!values["paid"] || len(confirmed) != 1 || confirmed[0] != "paid" {
		panic("CLI changed the checked provider switch owner")
	}
	host.writes++
	return sdk.ConfigSnapshot{Version: "4", Values: json.RawMessage(`{"paid":true}`)}, nil
}

func TestSDKProviderOptionsCLIRequiresCheckedBooleanUpdate(test *testing.T) {
	host := &providerOptionsCLIHost{}
	var output bytes.Buffer
	if err := sdkPluginRun(test.Context(), []string{"provider-options", "fixture", "vendor"}, host, &output, sdkCLIAuth{}); err != nil {
		test.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"configVersion": "3"`)) || host.writes != 0 {
		test.Fatalf("CLI option read changed the module: %s", &output)
	}
	output.Reset()
	args := []string{"provider-options", "fixture", "vendor",
		`{"packageRevision":"package","expected":"3","values":{"paid":true},"confirmed":["paid"]}`}
	if err := sdkPluginRun(test.Context(), args, host, &output, sdkCLIAuth{}); err != nil {
		test.Fatal(err)
	}
	if host.writes != 1 || !bytes.Contains(output.Bytes(), []byte(`"version": "4"`)) {
		test.Fatalf("CLI did not save the checked update: %s", &output)
	}
	for _, raw := range []string{
		`{"packageRevision":"package","expected":"3","values":{"paid":null}}`,
		`{"packageRevision":"package","expected":"3","values":{"paid":"true"}}`,
		`{"packageRevision":"package","expected":"3","values":{"paid":1}}`,
		`{"packageRevision":"package","expected":"3","values":{"paid":true},"unexpected":true}`,
		`{} {}`,
	} {
		args[3] = raw
		if err := sdkPluginRun(test.Context(), args, host, &output, sdkCLIAuth{}); err == nil {
			test.Fatalf("CLI accepted an ambiguous option update: %s", raw)
		}
	}
	if host.writes != 1 || host.closed != 7 {
		test.Fatalf("invalid CLI input reached configuration or leaked its host: writes=%d closed=%d", host.writes, host.closed)
	}
}
