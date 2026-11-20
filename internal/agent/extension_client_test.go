package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/usage"
)

func TestExtensionClientRecognizedAfterGatewayStarts(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.UA = []string{"launch-wire-fixture"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	host, source := prepareAgentWorkerFixture(t, ctx, descriptor, "../extension/testdata/actionworker", 5)
	if got := usage.AgentOf("codex/1.0"); got != "codex" {
		t.Fatalf("built-in agent before installation = %q", got)
	}
	handler := gateway.New().Front()
	check := func(id string, expected int) { checkAgentClientRoute(t, handler, id, expected) }

	check(descriptor.ID, http.StatusNotFound)
	check(descriptor.Aliases[0], http.StatusNotFound)
	if _, err := host.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	check(descriptor.ID, http.StatusOK)
	check(descriptor.Aliases[0], http.StatusOK)
	if got := usage.AgentOf("Launch-Wire-Fixture/2.0"); got != descriptor.ID {
		t.Errorf("SDK User-Agent resolved to %q, want %q", got, descriptor.ID)
	}
	if got := usage.AgentName(descriptor.ID); got != descriptor.Name {
		t.Errorf("SDK agent name = %q, want %q", got, descriptor.Name)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	check(descriptor.ID, http.StatusNotFound)
	check(descriptor.Aliases[0], http.StatusNotFound)
	if got := usage.AgentOf("Launch-Wire-Fixture/2.0"); got == descriptor.ID {
		t.Errorf("disabled SDK module still owns its User-Agent")
	}
	if got := usage.AgentName(descriptor.ID); got != descriptor.ID {
		t.Errorf("disabled SDK module still supplies name %q", got)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", true); err != nil {
		t.Fatal(err)
	}
	check(descriptor.ID, http.StatusOK)
	oldAlias := descriptor.Aliases[0]
	descriptor.Aliases = []string{"fixture-new"}
	descriptor.UA = []string{"fixture-wire-new"}
	descriptor.Name = "Replacement agent fixture"
	replaceAgentFixture(t, host, descriptor, "1.0.1", 5)
	check(oldAlias, http.StatusNotFound)
	check(descriptor.Aliases[0], http.StatusOK)
	if got := usage.AgentOf("Fixture-Wire-New/2.0"); got != descriptor.ID {
		t.Errorf("replacement User-Agent resolved to %q, want %q", got, descriptor.ID)
	}
	if got := usage.AgentOf("Launch-Wire-Fixture/2.0"); got == descriptor.ID {
		t.Errorf("old User-Agent still names the replacement")
	}
	if got := usage.AgentName(descriptor.ID); got != descriptor.Name {
		t.Errorf("replacement SDK agent name = %q, want %q", got, descriptor.Name)
	}
	if err := host.Remove(t.Context(), "fixture.agent"); err != nil {
		t.Fatal(err)
	}
	check(descriptor.ID, http.StatusNotFound)
}

func TestLegacyClientAliasRecognizedAfterGatewayStarts(t *testing.T) {
	if got := usage.AgentOf("codex/1.0"); got != "codex" {
		t.Fatalf("built-in agent before installation = %q", got)
	}
	handler := gateway.New().Front()
	pluggedHome(t, strings.Replace(aiderSrc, `id: "aider"`, `id: "aider", aliases: ["fixture-legacy"]`, 1))
	checkAgentClientRoute(t, handler, "fixture-legacy", http.StatusOK)
	if got := usage.AgentOf("fixture-legacy"); got != "aider" {
		t.Errorf("legacy alias resolved to %q, want aider", got)
	}
}

func checkAgentClientRoute(t *testing.T, handler http.Handler, id string, expected int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agent/"+id+"/v1/models", nil)
	request.RemoteAddr = "127.0.0.1:61234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != expected {
		t.Errorf("agent %s got HTTP %d, want %d: %s", id, response.Code, expected, response.Body)
	}
}
