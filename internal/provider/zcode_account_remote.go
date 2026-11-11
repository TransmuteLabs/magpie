package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/yetone/magpie/internal/plugin"
)

func zcodeRemotePost(ctx context.Context, p Provider, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, RemoteResetWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Anthropic+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	p.signRemote(req, p.Key)
	res, err := p.Do(http.DefaultClient, req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("remote native operation outcome is unknown; inspect its account before retrying: %w", ctx.Err())
		}
		return fmt.Errorf("remote native operation outcome is unknown; inspect its account before retrying: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("remote native operation outcome is unknown: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var response struct {
			Error struct {
				Code, Message string
				Account       *plugin.AccountError `json:"account"`
			} `json:"error"`
			State json.RawMessage `json:"state"`
		}
		_ = json.Unmarshal(b, &response)
		if len(response.State) != 0 && string(response.State) != "null" {
			_ = json.Unmarshal(response.State, out)
		}
		switch {
		case res.StatusCode == http.StatusUnauthorized:
			return errors.New(errRemoteKey)
		case res.StatusCode == http.StatusForbidden && response.Error.Code == KeyReadOnlyCode:
			return errors.New(errRemoteReadOnly)
		case res.StatusCode == http.StatusForbidden && response.Error.Code == KeyHeldCode:
			return errKeyHeld
		case response.Error.Account != nil:
			response.Error.Account.Message = response.Error.Message
			return response.Error.Account
		case response.Error.Message != "":
			return fmt.Errorf("%s: %s", remoteName(p), response.Error.Message)
		default:
			return fmt.Errorf("%s answered %s", remoteName(p), res.Status)
		}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("remote native operation returned an unreadable outcome; inspect its account before retrying")
	}
	return nil
}

func zcodeRemotePath(ctx context.Context, path string, in map[string]any) (string, error) {
	if expectation, ok := expectedZCodeAccount(ctx); ok {
		if expectation.scope == "" || expectation.generation == "" {
			return "", &plugin.AccountError{Kind: "invalid_context", Message: "both account scope and generation are required"}
		}
		in["scope"], in["generation"] = expectation.scope, expectation.generation
		return path + "/scoped", nil
	}
	if in["operation"] == "context" || in["status"] == true {
		return path, nil
	}
	return "", &plugin.AccountError{Kind: "invalid_context", Message: "capture both account scope and generation before the operation"}
}

// UseRemoteZCodeReset executes the action at the account owner and never
// transports vendor credentials. resolve acknowledges an unknown outcome.
func UseRemoteZCodeReset(ctx context.Context, id, account, window string, resolve bool) (ZCodeResetState, error) {
	p, card, ok := remoteOf(id)
	if !ok || card == "" || strings.Contains(card, "/") || strings.TrimSpace(account) == "" {
		return ZCodeResetState{}, errors.New("name a remote provider and its account")
	}
	if resolve && window != "" || !resolve && window != "five-hour" && window != "week" {
		return ZCodeResetState{}, errors.New("choose an explicit five-hour or week reset, or resolve an unknown outcome")
	}
	var out ZCodeResetState
	in := map[string]any{"provider": card, "account": account, "window": window, "resolve": resolve}
	path, err := zcodeRemotePath(ctx, RemoteZCodeResetPath, in)
	if err != nil {
		return out, err
	}
	if err := zcodeRemotePost(ctx, p, path, in, &out); err != nil {
		return out, err
	}
	return out, nil
}

// RemoteZCodeResetStatus reads status at the owner without spending.
func RemoteZCodeResetStatus(ctx context.Context, id, account string) (ZCodeResetState, error) {
	p, card, ok := remoteOf(id)
	if !ok || card == "" || strings.Contains(card, "/") || strings.TrimSpace(account) == "" {
		return ZCodeResetState{}, errors.New("name a remote provider and its account")
	}
	var out ZCodeResetState
	in := map[string]any{"provider": card, "account": account, "status": true}
	path, err := zcodeRemotePath(ctx, RemoteZCodeResetPath, in)
	if err != nil {
		return out, err
	}
	err = zcodeRemotePost(ctx, p, path, in, &out)
	return out, err
}

// RemoteZCodeAccountOperation sends only the public operation and its
// nonsecret parameters; the remote host resolves its own account.
func RemoteZCodeAccountOperation(ctx context.Context, id, account, operation string, params any) (plugin.AccountResult, error) {
	p, card, ok := remoteOf(id)
	if !ok || card == "" || strings.Contains(card, "/") || strings.TrimSpace(account) == "" {
		return plugin.AccountResult{}, errors.New("name a remote provider and its account")
	}
	var out plugin.AccountResult
	in := map[string]any{"provider": card, "account": account, "operation": operation, "params": params}
	path, err := zcodeRemotePath(ctx, RemoteZCodeAccountPath, in)
	if err != nil {
		return out, err
	}
	if err := zcodeRemotePost(ctx, p, path, in, &out); err != nil {
		return out, err
	}
	if expectation, ok := expectedZCodeAccount(ctx); ok && operation != "selection.select" {
		if out.Context.Scope != expectation.scope {
			return plugin.AccountResult{}, &plugin.AccountError{Kind: "account_scope_changed", Message: "remote native account scope changed"}
		}
		if out.Context.Generation != expectation.generation {
			return plugin.AccountResult{}, &plugin.AccountError{Kind: "account_generation_changed", Message: "remote native account login generation changed"}
		}
	}
	return out, nil
}
