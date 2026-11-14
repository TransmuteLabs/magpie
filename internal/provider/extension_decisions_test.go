package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func fixtureDecisionProvider(test *testing.T) *fixtureExtensionProviderHost {
	test.Helper()
	host := fixtureExtensionProvider(test, true)
	host.manifest.Contract.Version.Minor = 7
	if err := json.Unmarshal([]byte(`{"id":"vendor","name":"Judge","protocols":["chat"],"operations":["systemone"],
		"models":[{"id":"decision","kind":"decision","operations":["systemone"]},{"id":"conversation","kind":"chat"}]}`),
		&host.manifest.Providers[0]); err != nil {
		test.Fatal(err)
	}
	return host
}

func TestExtensionDecisionModelNeverInheritsConversationalProtocols(test *testing.T) {
	host := fixtureDecisionProvider(test)
	binding := extensionAccounts()[0].Account.extension
	for _, form := range []string{"missing", "null", "empty"} {
		test.Run(form, func(test *testing.T) {
			raw := `{"id":"decision","kind":"decision","operations":["systemone"]`
			if form == "null" {
				raw += `,"protocols":null`
			} else if form == "empty" {
				raw += `,"protocols":[]`
			}
			var model sdk.Model
			if err := json.Unmarshal([]byte(raw+"}"), &model); err != nil {
				test.Fatal(err)
			}
			projected, err := binding.projectModels([]sdk.Model{model, host.manifest.Providers[0].Models[1]})
			if err != nil || len(projected) != 2 || !projected[0].Decides || len(projected[0].APIs) != 0 ||
				!slices.Equal(projected[1].APIs, []string{"chat"}) {
				test.Fatalf("decision inherited chat or changed legacy defaults: %+v %v", projected, err)
			}
		})
	}
}

func TestExtensionDecisionEntitlementUsesSelectedCatalogAndColdEmpty(test *testing.T) {
	host := fixtureDecisionProvider(test)
	var executions atomic.Int32
	host.call = func(_ context.Context, method string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method == sdk.MethodProviderModels {
			if scope.Account == "alice" {
				return json.RawMessage(`{"models":[{"id":"decision","kind":"decision","operations":["systemone"]}]}`), nil
			}
			return json.RawMessage(`{"models":[]}`), nil
		}
		executions.Add(1)
		request := params.(sdk.ExecuteRequest)
		if method != sdk.MethodProviderExecute || request.Protocol != "" || request.Operation != "systemone" || request.Model != "decision" ||
			scope.Account != "alice" || request.Headers.Get("Authorization") != "" {
			test.Errorf("decision selected dispatch lost scope/envelope: %s %+v %+v", method, scope, request)
		}
		return json.RawMessage(`{"status":200,"outcome":"completed"}`), nil
	}
	providers, err := ExtensionProviders()
	if err != nil || len(providers) != 2 {
		test.Fatalf("decision accounts: %v %v", providers, err)
	}
	if providers[0].DecidesModel("decision") || providers[1].DecidesModel("decision") {
		test.Fatal("unknown account catalog borrowed a static decision hint")
	}
	for _, selected := range providers {
		if _, err := selected.Account.extension.fetchModels(test.Context()); err != nil {
			test.Fatal(err)
		}
	}
	primary := extensionAccounts()[0]
	if !primary.DecidesModel("decision") {
		test.Fatal("discovered decision model was not served")
	}
	all, _ := ExtensionProviders()
	for _, selected := range all {
		want := selected.Account.User == "alice"
		if selected.DecidesModel("decision") != want || selected.DecidesModel("unknown") {
			test.Fatalf("account %s borrowed another model/catalog", selected.Account.User)
		}
		request, _ := http.NewRequestWithContext(test.Context(), http.MethodPost,
			"extension://"+selected.ID+"/v1/systemone", strings.NewReader(`{"model":"decision","questions":{}}`))
		request.Header.Set("Authorization", "Bearer caller-only")
		response, err := selected.Do(http.DefaultClient, request)
		if want {
			if err != nil {
				test.Fatal(err)
			}
			_, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				test.Fatalf("decision completion: %v %v", readErr, closeErr)
			}
		} else if err == nil {
			response.Body.Close()
			test.Fatal("known-empty selected account executed another account's decision")
		}
	}
	extensionModelCache.Clear()
	cold, _ := ExtensionProviders()
	if cold[1].DecidesModel("decision") || executions.Load() != 1 {
		test.Fatalf("cold empty catalog gained a decision or dispatched: calls=%d", executions.Load())
	}
}
