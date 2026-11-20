package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionWindowAuthHost interface {
	CoreAuthStart(context.Context, string, string, string, string, sdk.WindowAuthStartOptions) (sdk.WindowAuthFlowView, error)
	CoreAuthFinish(context.Context, string, string, string, string, string) (sdk.WindowAuthFlowView, error)
	CoreAuthCancel(context.Context, string, string, string, string, string) (sdk.WindowAuthFlowView, error)
}

type extensionAuthRequest struct {
	Parent    string                     `json:"parent"`
	Request   string                     `json:"request"`
	Operation string                     `json:"operation"`
	Flow      string                     `json:"flow,omitempty"`
	Options   sdk.WindowAuthStartOptions `json:"options,omitempty"`
}

func readExtensionAuthRequest(w http.ResponseWriter, r *http.Request, body any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, windowRequestLimit))
	decoder.DisallowUnknownFields()
	if decoder.Decode(body) != nil || decoder.Decode(new(any)) != io.EOF {
		fail(w, errors.New("module authentication requires a bounded request"))
		return false
	}
	return true
}

func runExtensionAuth(w http.ResponseWriter, ctx context.Context, host extensionWindowAuthHost, id, revision, part, binding string, in extensionAuthRequest) {
	ctx = extension.WithWindowAuthParent(ctx, in.Parent)
	ctx = extension.WithModuleRequestID(ctx, in.Request)
	ctx = provider.ExtensionWindowAuthContext(ctx)
	var view sdk.WindowAuthFlowView
	var err error
	switch in.Operation {
	case "start":
		if in.Flow != "" {
			err = errors.New("start does not accept a flow handle")
		} else {
			view, err = host.CoreAuthStart(ctx, id, revision, part, binding, in.Options)
		}
	case "finish", "cancel":
		if in.Options.Method != "" || in.Options.Account != nil || len(in.Options.Input) != 0 || in.Flow == "" {
			err = errors.New("finish and cancel accept only a flow handle")
		} else if in.Operation == "finish" {
			view, err = host.CoreAuthFinish(ctx, id, revision, part, binding, in.Flow)
		} else {
			view, err = host.CoreAuthCancel(ctx, id, revision, part, binding, in.Flow)
		}
	default:
		err = errors.New("module authentication operation is not supported")
	}
	if err != nil || view.Validate() != nil {
		fail(w, errors.New("module authentication could not complete this request"))
		return
	}
	if part == sdk.ProviderSignInPart {
		writeJSON(w, struct {
			sdk.WindowAuthFlowView
			LinkOnly bool `json:"linkOnly,omitempty"`
		}{view, settings.Shared().SignInLinkOnly})
		return
	}
	writeJSON(w, view)
}

func extensionWindowAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/plugin-pages/auth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in struct {
			Ref     string `json:"ref"`
			Binding string `json:"binding"`
			extensionAuthRequest
		}
		if !readExtensionAuthRequest(w, r, &in) {
			return
		}
		part, err := resolveExtensionWindow(r.Context(), in.Ref)
		if err != nil {
			fail(w, errors.New("module authentication view is unavailable"))
			return
		}
		host, supported := extensionWindows().(extensionWindowAuthHost)
		if !supported {
			fail(w, errors.New("module authentication is unavailable"))
			return
		}
		runExtensionAuth(w, r.Context(), host, part.id, part.revision, part.part, in.Binding, in.extensionAuthRequest)
	})
}
