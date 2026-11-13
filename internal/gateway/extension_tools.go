package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionToolAccountSource interface {
	List() ([]extension.Installed, error)
	Store() *extension.Store
}

var extensionToolAccounts = func() extensionToolAccountSource { return extension.Default() }

var runExtensionPluginTool = plugin.RunExtensionTool

// extensionToolHTTPContext replaces inherited authority using authenticated
// request provenance. Client roots, sessions, arguments and IDs grant nothing.
func extensionToolHTTPContext(request *http.Request) context.Context {
	origin := gatewayOrigin(request)
	owner := (origin == "local" || origin == "magpie-key") && ownComputer(request) &&
		!hubFromPage(request) && !forwarded(request)
	return extension.WithLocalOwner(request.Context(), owner)
}

// extensionToolContext consumes optional _magpie: {provider, account}.
// Authors declare that optional property in their tool's input schema; only
// the host resolves its generation and removes it before calling the module.
func extensionToolContext(ctx context.Context, spec string, args json.RawMessage) (context.Context, json.RawMessage, error) {
	if !strings.HasPrefix(spec, plugin.ExtensionToolPrefix) {
		return ctx, args, nil
	}
	id, valid := plugin.ExtensionToolID(spec)
	if !valid {
		return ctx, nil, errors.New("invalid extension tool spec")
	}
	scope := sdk.Scope{Plugin: id, Session: extension.ToolScopeFrom(ctx).Session}
	if caller, ok := ctx.Value(callerCtx{}).(caller); ok && caller.session != "" {
		scope.Session = caller.session
	}
	if scope.Session == "" {
		scope.Session = HubCallerOf(ctx).Session
	}
	if len(bytes.TrimSpace(args)) == 0 || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		args = json.RawMessage(`{}`)
	}
	var arguments map[string]json.RawMessage
	if len(args) > toolsMCPBody || json.Unmarshal(args, &arguments) != nil || arguments == nil {
		return ctx, nil, errors.New("extension tool arguments must be a bounded JSON object")
	}
	requested, bound := arguments["_magpie"]
	if bound {
		who := access.Caller(ctx)
		for held := &who; held != nil; held = held.Issuer {
			if held.ReadOnly || held.Restricted() || len(held.Limits()) != 0 {
				return ctx, nil, errors.New("this gateway key cannot delegate extension account credentials")
			}
		}
		var selection struct {
			Provider string `json:"provider"`
			Account  string `json:"account"`
		}
		decoder := json.NewDecoder(bytes.NewReader(requested))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&selection) != nil || selection.Provider == "" || selection.Account == "" {
			return ctx, nil, errors.New("extension account context needs an explicit provider and account")
		}
		manager := extensionToolAccounts()
		entries, err := manager.List()
		if err != nil {
			return ctx, nil, errors.New("extension account declarations could not be read")
		}
		declared := false
		for _, entry := range entries {
			if entry.Disabled || entry.Manifest.ID != id || !entry.Manifest.Contract.Has(sdk.AccountAuth) ||
				!entry.Manifest.Contract.Has(sdk.ToolProvider) {
				continue
			}
			for _, provider := range entry.Manifest.Providers {
				declared = declared || provider.ID == selection.Provider
			}
		}
		if !declared {
			return ctx, nil, errors.New("extension does not declare authentication for this provider")
		}
		account, err := manager.Store().GetAccount(id, selection.Provider, selection.Account)
		if err != nil {
			return ctx, nil, errors.New("extension account is unavailable")
		}
		scope.Provider, scope.Account, scope.Generation = account.Provider, account.ID, account.Generation
		ctx, err = provider.ExtensionNetworkContext(ctx, scope)
		if err != nil {
			return ctx, nil, err
		}
		delete(arguments, "_magpie")
		args, err = json.Marshal(arguments)
		if err != nil {
			return ctx, nil, errors.New("extension tool arguments could not be prepared")
		}
	}
	metadata := extension.MetadataFrom(ctx)
	if metadata.Plugin == id && metadata.RequestID != "" {
		identity, err := json.Marshal([]string{scope.Plugin, scope.Provider, scope.Account,
			metadata.CallerID, scope.Session, metadata.RequestID})
		if err != nil {
			return ctx, nil, err
		}
		digest := sha256.Sum256(identity)
		scope.Operation, scope.Attempt = "request-"+hex.EncodeToString(digest[:]), 1
	}
	return extension.WithToolScope(ctx, scope), args, nil
}

// RunPluginToolResultOf is the Library's SDK result path, under the same
// plugin/tool and account checks as its text path. The hub reserves the call.
func RunPluginToolResultOf(ctx context.Context, name, tool string, args json.RawMessage, agent string) (json.RawMessage, error) {
	if !strings.HasPrefix(name, plugin.ExtensionToolPrefix) {
		text, err := RunPluginToolOf(ctx, name, tool, args, agent)
		if err != nil {
			return nil, err
		}
		return json.Marshal(toolResult(text, false))
	}
	spec, err := selectedPluginTool(ctx, name, tool)
	if err != nil {
		return nil, err
	}
	ctx, args, err = extensionToolContext(ctx, spec, args)
	if err != nil {
		return nil, err
	}
	result, err := runExtensionPluginTool(ctx, spec, tool, args)
	if err != nil {
		return nil, err
	}
	response, err := extensionMCPResult(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

func extensionMCPResult(result sdk.ToolResult) (map[string]any, error) {
	var blocks []json.RawMessage
	if json.Unmarshal(result.Content, &blocks) == nil && blocks != nil {
		for _, block := range blocks {
			var content struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(block, &content) != nil || content.Type == "" {
				return nil, errors.New("extension returned invalid MCP content")
			}
		}
		return map[string]any{"content": result.Content, "isError": result.IsError}, nil
	}
	if !json.Valid(result.Content) {
		return nil, errors.New("extension returned invalid tool content")
	}
	var text string
	if json.Unmarshal(result.Content, &text) != nil {
		text = string(result.Content)
	}
	return toolResult(text, result.IsError), nil
}
