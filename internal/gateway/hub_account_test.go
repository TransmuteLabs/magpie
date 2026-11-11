package gateway

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
)

// The real hub guard must reject account-bound services for remote and
// locally held keys while leaving an ordinary foreign HTTP server alone.
func TestHubMCPAccountOwnerPolicy(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	t.Setenv("MAGPIE_TRUST_PROXY", "")
	servers := []HubServer{{Name: "account", HereOnly: true, OwnerIdentity: true}, {Name: "docs"}}
	tools := []HubTool{
		{Server: "account", Tool: "do", Spec: hubSpec("do")},
		{Server: "docs", Tool: "do", Spec: hubSpec("do")},
	}
	f := &fakeHub{on: true, servers: map[string][]HubServer{"": servers}, tools: map[string][]HubTool{"": tools}}
	withHub(t, f)
	h := lanGuard(New().Handler())
	keys, secrets := newCaller(t, "Full", "Held", "Read only")
	if _, err := access.Update("models-key", access.Change{Key: keys[1].ID, Models: []string{"some/model"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("access-key", access.Change{Key: keys[2].ID, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	auth := func(i int) []string { return []string{"Authorization", "Bearer " + secrets[i]} }
	list := func(addr string, headers ...string) []string {
		t.Helper()
		m := readMCP(t, hubPostAt(h, HubMCPAll, addr, toolsList, headers...))
		var names []string
		for _, x := range m.Result.Tools {
			names = append(names, strings.TrimSuffix(x.Name, hubSep+"do"))
		}
		slices.Sort(names)
		return names
	}
	for _, tc := range []struct {
		name, addr string
		headers    []string
		want       []string
	}{
		{"local", here, nil, []string{"account", "docs"}},
		{"local full", here, auth(0), []string{"account", "docs"}},
		{"local held", here, auth(1), []string{"docs"}},
		{"remote full", "192.0.2.7:4000", auth(0), []string{"docs"}},
		{"remote held", "192.0.2.7:4000", auth(1), []string{"docs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := list(tc.addr, tc.headers...); !slices.Equal(got, tc.want) {
				t.Fatalf("listed %v, want %v", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		addr    string
		headers []string
	}{{here, auth(1)}, {"192.0.2.7:4000", auth(0)}, {"192.0.2.7:4000", auth(1)}} {
		m := readMCP(t, hubPostAt(h, HubMCPAll, tc.addr, toolsCall("account__do", `{}`), tc.headers...))
		if m.Error == nil && !m.Result.IsError {
			t.Fatal("held or remote key invoked owner account")
		}
	}
	if len(f.ran()) != 0 {
		t.Fatal("a denied account tool reached its server")
	}
	if w := hubPostAt(h, HubMCPAll, here, toolsList, auth(2)...); w.Code != http.StatusForbidden {
		t.Fatalf("read-only key listed MCP tools: %d", w.Code)
	}
	if w := hubPostAt(h, HubMCPAll, here, toolsCall("account__do", `{}`), auth(2)...); w.Code != http.StatusForbidden {
		t.Fatalf("read-only key invoked an MCP tool: %d", w.Code)
	}
	m := readMCP(t, hubPostAt(h, HubMCPAll, "192.0.2.7:4000", toolsCall("docs__do", `{}`), auth(1)...))
	if m.Error != nil || m.Result.IsError {
		t.Fatalf("ordinary HTTP server no longer works for a held remote key: %+v", m)
	}
}
