package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

// zcodeTaskID is the exact value the gateway can preserve as its session
// header. This GUI-only guard does not change the CLI's task semantics.
func zcodeTaskID(id string) bool {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.TrimSpace(id) != id {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

func zcodeAccountError(w http.ResponseWriter, err error) {
	var accountErr *plugin.AccountError
	if !errors.As(err, &accountErr) {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "accountError": accountErr})
}

func zcodeAccountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/usage/zcode-accounts", func(w http.ResponseWriter, r *http.Request) {
		accounts, available := provider.ZCodeAccountChoices()
		writeJSON(w, map[string]any{"available": available, "accounts": accounts})
	})
	mux.HandleFunc("POST /api/usage/zcode-account", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Provider   string          `json:"provider"`
			Account    string          `json:"account"`
			Operation  string          `json:"operation"`
			Params     json.RawMessage `json:"params"`
			Scope      string          `json:"scope"`
			Generation string          `json:"generation"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Provider) == "" || strings.TrimSpace(in.Account) == "" || in.Operation == "" {
			fail(w, errors.New("name a provider, account and operation"))
			return
		}
		if (in.Operation != "context" || in.Scope != "" || in.Generation != "") && (in.Scope == "" || in.Generation == "") {
			fail(w, errors.New("both account scope and generation are required"))
			return
		}
		if in.Operation == "execution.take" || in.Operation == "execution.draw" || in.Operation == "execution.continue" {
			var params struct {
				TaskID string `json:"taskId"`
			}
			if json.Unmarshal(in.Params, &params) != nil || !zcodeTaskID(params.TaskID) {
				fail(w, errors.New("taskId must be an exact, nonempty session header of at most 128 UTF-8 bytes without control bytes"))
				return
			}
		}
		var reset struct {
			Window string `json:"window"`
		}
		if strings.HasPrefix(in.Operation, "reset.") {
			if len(in.Params) != 0 && json.Unmarshal(in.Params, &reset) != nil {
				fail(w, errors.New("invalid reset parameters"))
				return
			}
			if in.Operation != "reset.status" && in.Operation != "reset.use" && in.Operation != "reset.resolve" ||
				in.Operation == "reset.use" && reset.Window != "five-hour" && reset.Window != "week" ||
				in.Operation != "reset.use" && reset.Window != "" {
				fail(w, errors.New("choose reset status, resolve, or an explicit five-hour or week window"))
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), provider.RemoteResetWait)
		defer cancel()
		if in.Scope != "" {
			ctx = provider.WithZCodeAccountExpectation(ctx, in.Scope, in.Generation)
		}
		remote := provider.IsRemoteCard(in.Provider)
		call := func(operation string, params any) (plugin.AccountResult, error) {
			if remote {
				return provider.RemoteZCodeAccountOperation(ctx, in.Provider, in.Account, operation, params)
			}
			return provider.ZCodeAccountOperation(ctx, in.Provider, in.Account, operation, params)
		}
		if !strings.HasPrefix(in.Operation, "reset.") {
			out, err := call(in.Operation, in.Params)
			if err != nil {
				zcodeAccountError(w, err)
				return
			}
			writeJSON(w, out)
			return
		}
		// Capture through the same fence before entering the reset owner.
		// Its coordinator captures again before any vendor operation.
		out, err := call("context", nil)
		if err != nil {
			zcodeAccountError(w, err)
			return
		}
		var data any
		switch in.Operation {
		case "reset.status":
			var state provider.ZCodeResetState
			if remote {
				state, err = provider.RemoteZCodeResetStatus(ctx, in.Provider, in.Account)
			} else {
				state, err = provider.ZCodeResetStatus(ctx, in.Provider, in.Account, false)
			}
			if err != nil && state.Source != "" && !provider.ZCodeAccountChanged(err) {
				err = nil // the coordinator returned its durable unavailable state
			}
			data = state
		case "reset.use":
			var state provider.ZCodeResetState
			if remote {
				state, err = provider.UseRemoteZCodeReset(ctx, in.Provider, in.Account, reset.Window, false)
			} else {
				state, err = provider.UseZCodeReset(ctx, in.Provider, in.Account, reset.Window)
			}
			if err != nil && state.Pending != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "state": state})
				return
			}
			data = state
		case "reset.resolve":
			if remote {
				_, err = provider.UseRemoteZCodeReset(ctx, in.Provider, in.Account, "", true)
			} else {
				err = provider.ResolveZCodeReset(ctx, in.Provider, in.Account)
			}
			data = map[string]bool{"resolved": true}
		}
		if err != nil {
			zcodeAccountError(w, err)
			return
		}
		out.Data, err = json.Marshal(data)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, out)
	})
}
