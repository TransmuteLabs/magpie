package gui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestProvidersListSDKRetrievalApartFromChat(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "retrieval.fixture", Version: "1.0.0",
		Runtime: "exec", Entry: entry, Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 3},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Retrieval fixture", Protocols: []protocolext.Protocol{protocolext.Chat},
			Operations: []sdk.ProviderOperation{sdk.OperationEmbeddings, sdk.OperationRerank},
			Models:     []sdk.Model{{ID: "fixture-vector", Kind: "embedding"}, {ID: "fixture-ranker", Kind: "rerank"}}}}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, entry), []byte("must never execute"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := extension.NewCatalog(extension.Default().Namespace())
	if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	var state providersJSON
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil {
		t.Fatalf("/api/providers: %d %s", response.Code, response.Body)
	}
	id := provider.ExtensionProviderID(manifest.ID, "vendor")
	for _, entry := range state.Providers {
		if entry.ID != id {
			continue
		}
		if len(entry.Models) != 0 || entry.Retrieval != 2 ||
			!slices.Equal(entry.EmbedIDs, []string{"fixture-vector"}) ||
			!slices.Equal(entry.RerankIDs, []string{"fixture-ranker"}) {
			t.Errorf("SDK retrieval inventory: chat=%v, count=%d, embedding=%v, rerank=%v",
				entry.Models, entry.Retrieval, entry.EmbedIDs, entry.RerankIDs)
		}
		return
	}
	t.Fatalf("SDK provider %s absent", id)
}

// The add form's Fetch models (#578) asks the vendor for its list with the
// URL and key typed, before the provider is saved: the list of models
// agents chat with comes back, nothing is saved — no provider, no list
// kept — and a vendor refusing says why.
func TestProviderListBeforeSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var asked []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk-typed" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"id":"gpt-5.5"},{"id":"claude-sonnet-5"},{"id":"text-embedding-3"},{"id":"bge-reranker-v2-m3"}]}`)
	}))
	defer up.Close()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/list", strings.NewReader(body)))
		return w
	}
	w := post(`{"typed":true,"name":"Relay","key":"sk-typed","chat":"` + up.URL + `/v1"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got struct {
		Models    []struct{ ID, Name string }
		EmbedIDs  []string
		RerankIDs []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// listed apart by kind, for the form to name each one (heatonz;
	// solider1394 on X: a count alone read as no models)
	if strings.Join(got.EmbedIDs, " ") != "text-embedding-3" || strings.Join(got.RerankIDs, " ") != "bge-reranker-v2-m3" {
		t.Fatalf("embed %v rerank %v: %s", got.EmbedIDs, got.RerankIDs, w.Body)
	}
	var ids []string
	for _, m := range got.Models {
		ids = append(ids, m.ID)
	}
	// the models agents chat with: an embedding model is left out
	if s := strings.Join(ids, " "); s != "gpt-5.5 claude-sonnet-5" && s != "claude-sonnet-5 gpt-5.5" {
		t.Fatalf("listed %s", s)
	}
	if ps := provider.All(); len(ps) != 0 {
		t.Fatalf("list saved %v", ps)
	}
	for _, id := range []string{"", "relay"} {
		if ms, _, ok := catalog.Live(id); ok || len(ms) != 0 {
			t.Fatalf("list kept %q's: %v", id, ms)
		}
	}
	if !strings.Contains(strings.Join(asked, "\n"), "GET /v1/models Bearer sk-typed") {
		t.Fatalf("asked %v", asked)
	}

	// a key the vendor refuses: the form is told why
	w = post(`{"typed":true,"key":"sk-wrong","chat":"` + up.URL + `/v1"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "401") {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
	// no URL: nothing to ask
	if w = post(`{"typed":true,"key":"sk-typed"}`); w.Code != 400 {
		t.Fatalf("no URL: %d %s", w.Code, w.Body)
	}
}
