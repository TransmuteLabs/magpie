package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sdkCLIProviderOptions(ctx context.Context, args []string, host sdkCLIHost, out io.Writer) error {
	if len(args) < 2 || len(args) > 3 {
		return sdkCLIUsage("provider-options requires a module, provider and optional checked JSON update")
	}
	owner, ok := host.(interface {
		ProviderOptions(context.Context, string, string) (*extension.ProviderOptionsSnapshot, error)
		CompareAndSwapProviderOptions(context.Context, string, string, string, string, map[string]bool, []string) (sdk.ConfigSnapshot, error)
	})
	if !ok {
		return errors.New("provider switch configuration is unavailable")
	}
	if len(args) == 2 {
		options, err := owner.ProviderOptions(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		return sdkCLIJSON(out, options)
	}
	if len(args[2]) > 64<<10 {
		return errors.New("provider switch update is too large")
	}
	var update struct {
		PackageRevision string                    `json:"packageRevision"`
		Expected        string                    `json:"expected"`
		Values          sdk.ProviderOptionChanges `json:"values"`
		Confirmed       []string                  `json:"confirmed"`
	}
	decoder := json.NewDecoder(strings.NewReader(args[2]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("provider switch update must be one JSON object")
	}
	config, err := owner.CompareAndSwapProviderOptions(ctx, args[0], args[1], update.PackageRevision,
		update.Expected, update.Values, update.Confirmed)
	if err != nil {
		return err
	}
	return sdkCLIJSON(out, config)
}
