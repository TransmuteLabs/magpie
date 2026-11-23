package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/access"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModelsListConditionalFollowsForeignSDKConfig(t *testing.T) {
	const before, after = "old-entitlement", "new-entitlement"
	var updated atomic.Bool
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 {
			t.Error("models list attempted SDK inference")
			http.Error(w, "unexpected SDK execution", http.StatusBadGateway)
			return
		}
		model := before
		if updated.Load() {
			model = after
		}
		if err := json.NewEncoder(w).Encode(sdk.ModelsResult{Models: []sdk.Model{{ID: model}}}); err != nil {
			t.Error(err)
		}
	}, func(manifest *sdk.Manifest) {
		manifest.ID = codexEtagFixtureID
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ModelDiscovery)
		manifest.Providers[0].Models = nil
	})
	oldID, newID := id+"/"+before, id+"/"+after
	keys, secrets := newCaller(t, "Both entitlements", "Old entitlement")
	for index, models := range [][]string{{oldID, newID}, {oldID}} {
		if _, err := access.Update("models-key", access.Change{Key: keys[index].ID, Models: models}); err != nil {
			t.Fatal(err)
		}
	}

	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	ask := func(secret, tag string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/v1/models", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		req.Header.Set("Authorization", "Bearer "+secret)
		if tag != "" {
			req.Header.Set("If-None-Match", tag)
		}
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, req)
		return reply
	}
	const private = "private, no-cache"
	list := func(reply *httptest.ResponseRecorder, want ...string) string {
		t.Helper()
		tag := reply.Header().Get("ETag")
		if reply.Code != http.StatusOK || tag == "" || reply.Header().Get("Cache-Control") != private {
			t.Fatalf("models list: status %d, ETag %q, cache %q, want models %v, body %s",
				reply.Code, tag, reply.Header().Get("Cache-Control"), want, reply.Body)
		}
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(reply.Body.Bytes(), &payload); err != nil || payload.Data == nil {
			t.Fatalf("invalid models list: %v, body %s", err, reply.Body)
		}
		ids := make([]string, len(payload.Data))
		for index, model := range payload.Data {
			ids[index] = model.ID
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("listed models %v, want %v", ids, want)
		}
		return tag
	}
	unchanged := func(secret, tag string) {
		t.Helper()
		reply := ask(secret, tag)
		if reply.Code != http.StatusNotModified || reply.Header().Get("ETag") != tag ||
			reply.Header().Get("Cache-Control") != private || reply.Body.Len() != 0 {
			t.Fatalf("conditional models list: status %d, ETag %q (want %q), cache %q, body %q",
				reply.Code, reply.Header().Get("ETag"), tag, reply.Header().Get("Cache-Control"), reply.Body.String())
		}
	}

	oldTag := list(ask(secrets[0], ""), oldID)
	unchanged(secrets[0], oldTag)
	list(ask(secrets[1], ""), oldID)

	updated.Store(true)
	if output, err := publishForeignCodexModels(t.Context(), after, true); err != nil {
		t.Fatalf("foreign config and model publication: %v\n%s", err, output)
	}
	newTag := list(ask(secrets[0], oldTag), newID)
	if newTag == oldTag {
		t.Fatalf("new models list kept its old ETag %q", oldTag)
	}
	unchanged(secrets[0], newTag)
	deniedTag := list(ask(secrets[1], newTag))
	if deniedTag == newTag {
		t.Fatalf("restricted models list reused the other key's ETag %q", newTag)
	}
	unchanged(secrets[1], deniedTag)
}
