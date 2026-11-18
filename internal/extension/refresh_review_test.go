package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestRefreshAcceptsDeclaredCredentialBound(t *testing.T) {
	credentials := json.RawMessage(`{"token":"` + strings.Repeat("x", 2<<20) + `"}`)
	manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(sdk.AuthResult{ID: "account", Credentials: credentials})
	})
	if err := validateRefreshResult(scope, sdk.AuthResult{ID: scope.Account, Credentials: credentials}); err != nil {
		t.Fatal("fixture does not satisfy the declared credential bound:", err)
	}
	receipt, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
	account, readErr := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || readErr != nil || account.Generation != 2 || calls.Load() != 1 ||
		len(receipt.Credentials) != 0 || !bytes.Equal(account.Auth, credentials) {
		t.Fatalf("valid credential result did not publish: exchanges=%d generation=%d refresh=%v read=%v",
			calls.Load(), account.Generation, err, readErr)
	}
}

func TestRefreshShutdownCancelsOrdinaryRPC(t *testing.T) {
	manager, _ := fixtureManager(t)
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	ready, ended := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := manager.Call(caller, "fixture", sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"wait"}`)},
			func(event Event) error {
				if event.Name == "ready" {
					close(ready)
				}
				return nil
			})
		ended <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary fixture RPC never started")
	}
	closeContext, cancelClose := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelClose()
	if err := manager.Close(closeContext); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Error("unexpected Close error:", err)
	}
	select {
	case <-manager.closeDone:
	case <-time.After(2 * time.Second):
		t.Error("owned shutdown still waits for the ordinary caller to cancel")
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("ordinary RPC survived owned shutdown")
		cancelCaller()
		<-ended
	}
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	if err := manager.Close(cleanup); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshResultDecoderKeepsStrictEnvelopeValidation(t *testing.T) {
	scope := refreshScope()
	for _, raw := range []string{
		`null`,
		`{"id":"account","credentials":{}} trailing`,
		`{"id":"account","credentials":{},"unexpected":true}`,
		`{"id":"account","credentials":{},"expiresAt":9223372036854775808}`,
		`{"id":"account","credentials":null}`,
		`{"id":"foreign","credentials":{}}`,
		strings.Repeat(" ", storeFileLimit+1),
	} {
		if _, err := decodeRefreshResult(scope, json.RawMessage(raw)); err == nil {
			t.Error("invalid refresh result was accepted")
		}
	}
}
