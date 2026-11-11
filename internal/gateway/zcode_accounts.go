package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

func writeZCodeAccountError(w http.ResponseWriter, err error) {
	var accountErr *plugin.AccountError
	if errors.As(err, &accountErr) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": err.Error(), "account": accountErr,
		}})
		return
	}
	writeError(w, provider.Chat, http.StatusBadGateway, err.Error())
}

// Native account operations expose the owner's nonsecret control plane,
// never credentials. Like Codex reset, even local callers need a full key.
func zcodeAccountAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if !local(r) && !sharedWith(r) {
		writeError(w, provider.Chat, http.StatusForbidden, "native account controls are available only locally or when sharing is enabled")
		return false
	}
	who := access.Caller(r.Context())
	if who.KeyID == "" || who.ReadOnly || who.Browser || fromBrowser(r) {
		writeError(w, provider.Chat, http.StatusUnauthorized, "native account controls require an enabled gateway key with full access")
		return false
	}
	if refuseHeld(w, r, "this gateway key is held to a budget, models or accounts and cannot control native accounts") {
		return false
	}
	return true
}

func (s *Server) zcodeReset(w http.ResponseWriter, r *http.Request) {
	if !zcodeAccountAuthorized(w, r) {
		return
	}
	var in struct {
		Provider   string `json:"provider"`
		Account    string `json:"account"`
		Window     string `json:"window"`
		Resolve    bool   `json:"resolve"`
		Status     bool   `json:"status"`
		Scope      string `json:"scope"`
		Generation string `json:"generation"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || in.Provider == "" || in.Account == "" || strings.Contains(in.Provider, "/") {
		writeError(w, provider.Chat, http.StatusBadRequest, "name a local provider and its account")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if strings.HasSuffix(r.URL.Path, "/scoped") || !in.Status || in.Scope != "" || in.Generation != "" {
		if in.Scope == "" || in.Generation == "" {
			writeError(w, provider.Chat, http.StatusBadRequest, "both account scope and generation are required")
			return
		}
		ctx = provider.WithZCodeAccountExpectation(ctx, in.Scope, in.Generation)
	}
	if in.Status {
		if in.Resolve || in.Window != "" {
			writeError(w, provider.Chat, http.StatusBadRequest, "choose status, resolution or one reset window")
			return
		}
		out, err := provider.ZCodeResetStatus(ctx, in.Provider, in.Account, false)
		if err != nil {
			if out.Source != "" && !provider.ZCodeAccountChanged(err) {
				writeJSON(w, http.StatusOK, out)
				return
			}
			writeZCodeAccountError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if in.Resolve {
		if in.Window != "" {
			writeError(w, provider.Chat, http.StatusBadRequest, "resolution does not spend a reset window")
			return
		}
		if err := provider.ResolveZCodeReset(ctx, in.Provider, in.Account); err != nil {
			writeZCodeAccountError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"source": "zcode", "resolved": true})
		return
	}
	out, err := provider.UseZCodeReset(ctx, in.Provider, in.Account, in.Window)
	if err != nil {
		if out.Pending != "" {
			var accountErr *plugin.AccountError
			errors.As(err, &accountErr)
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"message": err.Error(), "account": accountErr}, "state": out,
			})
			return
		}
		writeZCodeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) zcodeAccount(w http.ResponseWriter, r *http.Request) {
	if !zcodeAccountAuthorized(w, r) {
		return
	}
	var in struct {
		Provider   string          `json:"provider"`
		Account    string          `json:"account"`
		Operation  string          `json:"operation"`
		Params     json.RawMessage `json:"params"`
		Scope      string          `json:"scope"`
		Generation string          `json:"generation"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || in.Provider == "" || in.Account == "" || in.Operation == "" || strings.Contains(in.Provider, "/") {
		writeError(w, provider.Chat, http.StatusBadRequest, "name a local provider, its account and an operation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if strings.HasSuffix(r.URL.Path, "/scoped") || in.Operation != "context" || in.Scope != "" || in.Generation != "" {
		if in.Scope == "" || in.Generation == "" {
			writeError(w, provider.Chat, http.StatusBadRequest, "both account scope and generation are required")
			return
		}
		ctx = provider.WithZCodeAccountExpectation(ctx, in.Scope, in.Generation)
	}
	out, err := provider.ZCodeAccountOperation(ctx, in.Provider, in.Account, in.Operation, in.Params)
	if err != nil {
		writeZCodeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
