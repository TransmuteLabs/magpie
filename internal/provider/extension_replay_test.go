package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionProviderNativeReplayAfterReauthKeepsOriginalOperation(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	var scopes []sdk.Scope
	executions := 0
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderExecute {
			t.Errorf("unexpected method: %s", method)
			return nil, errors.New("unexpected extension method")
		}
		scopes = append(scopes, scope)
		if _, err := host.store.Journal(scope, "dispatched"); err != nil {
			return nil, err
		}
		executions++
		if _, err := host.store.Journal(scope, "completed"); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":204,"outcome":"completed"}`), nil
	}
	native := extensionhost.WithRequestMetadata(context.Background(), extensionhost.RequestMetadata{
		Plugin: host.manifest.ID, CallerID: "host-caller", RequestID: "1",
	})
	send := func() (*ExtensionAttempt, error) {
		ctx, attempt := WithExtensionAttempt(native)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.Chat+"/chat/completions",
			strings.NewReader(`{"model":"m1"}`))
		if err != nil {
			return attempt, err
		}
		request.Header.Set(ConversationHeader, "native-session")
		response, err := provider.Do(http.DefaultClient, request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if err == nil {
				err = closeErr
			}
		}
		return attempt, err
	}
	if _, err := send(); err != nil || executions != 1 {
		t.Fatalf("first native operation: executions=%d error=%v", executions, err)
	}
	scope, _ := provider.ExtensionScope()
	if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: scope.Plugin, Provider: scope.Provider,
		ID: scope.Account, Auth: json.RawMessage(`{"access":"reauthenticated"}`)}, scope.Generation); err != nil {
		t.Fatal(err)
	}
	provider = extensionAccounts()[0]
	attempt, err := send()
	var conflict *sdk.Error
	if !errors.As(err, &conflict) || conflict.Code != "operation_conflict" || conflict.Outcome != "completed" ||
		executions != 1 || len(scopes) != 2 {
		t.Fatalf("reauthenticated native retry executed again: executions=%d scopes=%+v error=%v", executions, scopes, err)
	}
	if scopes[0].Operation != scopes[1].Operation || scopes[0].Generation == scopes[1].Generation {
		t.Fatalf("native identity or account generation was not preserved: %+v", scopes)
	}
	if attempt.Outcome(false) == nil {
		t.Fatal("completed conflict after reauthentication became eligible for fallback")
	}
	entry, err := host.store.Operation(scope.Plugin, scopes[0].Operation)
	if err != nil || entry.Scope != scopes[0] || entry.State != "completed" {
		t.Fatalf("retry replaced the original fenced journal scope: %+v %v", entry, err)
	}
}
