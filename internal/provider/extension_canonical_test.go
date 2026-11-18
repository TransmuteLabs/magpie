package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionProviderCanonicalCallerIdentity(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	selected := extensionAccounts()[0]
	var captured sdk.ExecuteOptions
	var capturedScope sdk.Scope
	host.call = func(_ context.Context, method string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderExecute {
			t.Errorf("unexpected method: %s", method)
		}
		request := params.(sdk.ExecuteRequest)
		capturedScope = scope
		captured = sdk.ExecuteOptions{}
		if err := json.Unmarshal(request.Options, &captured); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":204,"outcome":"completed"}`), nil
	}
	for _, sample := range []struct {
		name, caller, owner, nativeID string
	}{
		{name: "ordinary", caller: "trusted-key-a"},
		{name: "other caller", caller: "trusted-key-b"},
		{name: "local owner", caller: "trusted-local-owner"},
		{name: "foreign metadata", caller: "trusted-key-a", owner: "foreign.module", nativeID: "foreign-request"},
		{name: "same owner", caller: "trusted-key-a", owner: host.manifest.ID, nativeID: "native-request"},
		{name: "no trusted identity"},
		{name: "metadata cannot supply identity", owner: host.manifest.ID, nativeID: "untrusted-caller-request"},
	} {
		for _, endpoint := range []string{"/chat/completions", "/messages"} {
			t.Run(sample.name+endpoint, func(t *testing.T) {
				ctx := extensionhost.WithCallerID(t.Context(), sample.caller)
				ctx = extensionhost.WithRequestMetadata(ctx, extensionhost.RequestMetadata{
					Plugin: sample.owner, CallerID: "metadata-caller", RequestID: sample.nativeID,
					Value: json.RawMessage(`{"callerId":"native-spoof","private":"owner-only"}`),
				})
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+endpoint,
					strings.NewReader(`{"model":"m1","callerId":"body-spoof","options":{"callerId":"options-spoof","requestId":"body-request","native":{"private":true}}}`))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Magpie-Caller-ID", "header-spoof")
				request.Header.Set(ConversationHeader, "effective-session")
				response, err := selected.Do(http.DefaultClient, request)
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("response failed: %v %v", readErr, closeErr)
				}
				if captured.CallerID != sample.caller {
					t.Errorf("canonical caller identity = %q, want trusted context %q", captured.CallerID, sample.caller)
				}
				if capturedScope.Account != "alice" || capturedScope.Generation != 1 || capturedScope.Session != "effective-session" {
					t.Errorf("selected account/session scope changed: %+v", capturedScope)
				}
				if sample.owner == host.manifest.ID {
					if captured.RequestID != sample.nativeID || !strings.Contains(string(captured.Native), "owner-only") {
						t.Errorf("same-owner native metadata was lost: %+v", captured)
					}
				} else if captured.RequestID != "" || len(captured.Native) != 0 {
					t.Errorf("foreign or caller-supplied native metadata escaped: %+v", captured)
				}
			})
		}
	}
}
