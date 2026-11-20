package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/memory"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionMemoryOrdinaryMCP(t *testing.T) {
	fresh(t)
	const id = "fixture.memory"
	spec := plugin.ExtensionToolPrefix + id
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/memoryworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Memory fixture: %v\n%s", err, output)
	}
	schema := json.RawMessage(`{"type":"object"}`)
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: id, Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 14}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
		Tools: []sdk.ToolDescriptor{
			{Name: "recall", Exposure: "local-owner", MemorySearch: true, InputSchema: schema},
			{Name: "other", Exposure: "local-owner", InputSchema: schema},
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(gwtool.Serve(gwtool.PluginTools))
	previous := MCPHub
	MCPHub = exposureGatewayHub{spec: spec}
	t.Cleanup(func() { MCPHub = previous })

	project := memory.ProjectOf(t.TempDir())
	otherProject := memory.ProjectOf(t.TempDir())
	var tracked []string
	for _, fixture := range []struct {
		scope, suffix string
		project       *memory.Project
	}{
		{scope: memory.Global, suffix: "global"},
		{scope: memory.ScopeOf(project), suffix: "visible-project", project: project},
		{scope: memory.ScopeOf(otherProject), suffix: "hidden-project", project: otherProject},
		{scope: "agent:codex", suffix: "visible-agent"},
		{scope: "agent:other-fixture", suffix: "hidden-agent"},
	} {
		saved, _, err := memory.Save(memory.Input{Scope: new(fixture.scope), Kind: new("fact"), Text: new("checkpoint " + fixture.suffix)}, "user", nil, fixture.project)
		if err != nil {
			t.Fatal(err)
		}
		tracked = append(tracked, saved.ID)
	}
	handler := lanGuard(New().Handler())
	send := func(path, name string, times, limit int, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{
				"name": name, "arguments": map[string]any{
					"times": times, "search": map[string]any{"query": "checkpoint", "limit": limit},
					"project": otherProject.Key, "agent": "other-fixture",
				},
				"_meta": map[string]any{
					HubRootsMeta:  []string{fileURI(otherProject.Roots[0])},
					HubClientMeta: "other-fixture",
					"x-codex-turn-metadata": map[string]any{
						"workspaces": map[string]any{otherProject.Roots[0]: map[string]any{}},
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		headers = append([]string{"Accept", "application/json", MemoryClientHeader, "codex-mcp-client", MemoryCwdHeader, project.Roots[0]}, headers...)
		return mcpPostTo(handler, path, here, string(body), headers...)
	}
	post := func(path, name string, times, limit int, headers ...string) mcpReply {
		t.Helper()
		response := send(path, name, times, limit, headers...)
		if response.Code != http.StatusOK {
			t.Fatalf("MCP status %d: %s", response.Code, response.Body)
		}
		return readMCP(t, response)
	}
	checkScopes := func(path string, response mcpReply, want ...string) {
		t.Helper()
		if response.Error != nil || response.Result.IsError || len(response.Result.Content) != 1 {
			t.Fatalf("scoped Memory call at %s: %+v", path, response)
		}
		var result sdk.MemorySearchResult
		if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &result); err != nil {
			t.Fatal(err)
		}
		var seen []string
		for _, hit := range result.Hits {
			seen = append(seen, hit.Text)
		}
		slices.Sort(seen)
		slices.Sort(want)
		if !slices.Equal(seen, want) {
			t.Fatalf("%s returned the wrong Memory scopes: got %q, want %q", path, seen, want)
		}
	}
	for _, route := range []struct{ path, tool string }{
		{ToolsMCPPath, ToolPrefix(spec) + "_recall"},
		{ToolsMCPOne + ToolPrefix(spec), "recall"},
		{HubMCPAll, "sdk__recall"},
	} {
		checkScopes(route.path, post(route.path, route.tool, 1, 0),
			"checkpoint global", "checkpoint visible-project", "checkpoint visible-agent")
		checkScopes(route.path, post(route.path, route.tool, 1, 0, MemoryCwdHeader, ""),
			"checkpoint global", "checkpoint visible-agent")
		forwarded := send(route.path, route.tool, 1, 0, "X-Forwarded-For", "192.0.2.1")
		if forwarded.Code != http.StatusForbidden {
			t.Fatalf("%s answered a forwarded tool call: %d %s", route.path, forwarded.Code, forwarded.Body)
		}
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	url := server.URL + HubMCPAll
	const userAgent = "codex-mcp-client/1"
	stream := hubPostTo(t, url, toolsCall("sdk__recall", `{"times":1,"search":{"query":"checkpoint"}}`), bothAccept, userAgent)
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Memory roots stream: %d %s", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	events := hubEvents(t, stream)
	ask := nextEvent(t, events, "Memory roots/list")
	if ask.Method != "roots/list" || len(ask.ID) == 0 {
		t.Fatalf("Memory asked %+v, want roots/list", ask)
	}
	roots, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": ask.ID,
		"result": map[string]any{"roots": []map[string]string{{"uri": fileURI(project.Roots[0])}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := hubPostTo(t, url, string(roots), bothAccept, userAgent)
	if answer.StatusCode != http.StatusAccepted {
		t.Fatalf("Memory roots/list answer: %d", answer.StatusCode)
	}
	done := nextEvent(t, events, "Memory tool reply")
	raw, err := json.Marshal(done)
	if err != nil {
		t.Fatal(err)
	}
	var reply mcpReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	checkScopes(HubMCPAll, reply, "checkpoint global", "checkpoint visible-project", "checkpoint visible-agent")
	for _, failure := range []struct {
		name   string
		times  int
		limit  int
		header []string
	}{
		{name: "other", times: 1},
		{name: "recall", times: 5},
		{name: "recall", times: 1, limit: sdk.MaxMemorySearchHits + 1},
		{name: "recall", times: 1, header: []string{"User-Agent", "Mozilla/5.0"}},
		{name: "recall", times: 1, header: []string{RouterHeader, "1"}},
	} {
		response := post(ToolsMCPOne+ToolPrefix(spec), failure.name, failure.times, failure.limit, failure.header...)
		if response.Error == nil && !response.Result.IsError {
			t.Errorf("unexpected scoped Memory success: %+v", response)
		}
	}
	for _, id := range tracked {
		saved, err := memory.Get(id)
		if err != nil || saved == nil || saved.Uses != 0 || !saved.Used.IsZero() {
			t.Fatalf("scoped recall touched Memory use: %+v %v", saved, err)
		}
	}
	oldRanking := memory.SemanticRanker
	var revocationErr error
	memory.SemanticRanker = sdk.MemorySemanticRanker{
		Ready: func() bool { return true },
		Rank: func(sdk.MemorySemanticRequest) map[string]float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if revocationErr = manager.SetEnabled(ctx, id, false); revocationErr == nil {
				revocationErr = manager.SetEnabled(ctx, id, true)
			}
			return nil
		},
	}
	t.Cleanup(func() { memory.SemanticRanker = oldRanking })
	reply = post(ToolsMCPOne+ToolPrefix(spec), "recall", 1, 0)
	memory.SemanticRanker = oldRanking
	if revocationErr != nil || reply.Error == nil && !reply.Result.IsError {
		t.Fatalf("previous enablement returned Memory after disable/enable: %+v %v", reply, revocationErr)
	}
	if reply := post(ToolsMCPOne+ToolPrefix(spec), "recall", 1, 0); reply.Error != nil || reply.Result.IsError {
		t.Fatalf("new enablement could not read Memory: %+v", reply)
	}
	if err := manager.SetEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if response := post(ToolsMCPOne+ToolPrefix(spec), "recall", 1, 0); response.Error == nil && !response.Result.IsError {
		t.Fatal("disabled module still read Memory")
	}
}

func TestExtensionMemoryHitBudget(t *testing.T) {
	fresh(t)
	query := sdk.MemorySearchRequest{Query: "checkpoint", Limit: 3}
	for _, suffix := range []string{"one", "two", "three", "four"} {
		_, _, err := memory.Save(memory.Input{Kind: new("fact"), Text: new("checkpoint " + suffix)}, "user", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := searchExtensionMemory(t.Context(), memoryCaller{}, query)
	if err != nil || len(result.Hits) != query.Limit {
		t.Fatalf("unexpected bounded result: %+v %v", result, err)
	}
	if _, err := searchExtensionMemory(t.Context(), memoryCaller{}, sdk.MemorySearchRequest{
		Query: strings.Repeat("特", sdk.MaxMemorySearchQuery),
	}); err == nil {
		t.Fatal("oversized UTF-8 query was accepted")
	}
}
