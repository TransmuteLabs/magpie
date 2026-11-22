package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionOptionHost interface {
	ProviderOptions(context.Context, string, string) (*extension.ProviderOptionsSnapshot, error)
	CompareAndSwapProviderOptions(context.Context, string, string, string, string, map[string]bool, []string) (sdk.ConfigSnapshot, error)
}

type extensionProviderOptionsJSON struct {
	*extension.ProviderOptionsSnapshot
	OwnerRevision string `json:"ownerRevision,omitempty"`
}

var extensionProviderOptions = func() extensionOptionHost { return extension.Default() }

func extensionProviderOptionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/provider/extension-options", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ID              string                    `json:"id"`
			Module          string                    `json:"module"`
			Provider        string                    `json:"provider"`
			PackageRevision string                    `json:"packageRevision"`
			OwnerRevision   string                    `json:"ownerRevision,omitempty"`
			Expected        string                    `json:"expected"`
			Values          sdk.ProviderOptionChanges `json:"values"`
			Confirmed       []string                  `json:"confirmed"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			fail(w, err)
			return
		}
		id, _, visible := provider.ExtensionPublicProvider(input.Module, input.Provider)
		if decoder.Decode(new(any)) != io.EOF || !visible || input.ID != id ||
			provider.CheckExtensionSubscription(r.Context(), input.Module, input.Provider, input.OwnerRevision) != nil {
			fail(w, errors.New("provider switch update requires its exact SDK owner"))
			return
		}
		ctx := extension.WithSubscriptionOwnerRevision(r.Context(), input.OwnerRevision)
		config, err := extensionProviderOptions().CompareAndSwapProviderOptions(ctx, input.Module, input.Provider,
			input.PackageRevision, input.Expected, input.Values, input.Confirmed)
		if err != nil {
			fail(w, err)
			return
		}
		provider.Changed()
		writeJSON(w, struct {
			Version string `json:"version"`
		}{Version: config.Version})
	})
}
