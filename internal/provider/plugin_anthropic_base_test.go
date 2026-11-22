package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/testenv"
)

// A Claude model through the Copilot plugin goes to Copilot's
// /v1/messages: the loader's base is the bare https://api.githubcopilot.com
// for every model, and the model's own URL (models.dev's) says /v1. Sent to
// the bare base it went to /messages and Copilot answered 404 (cyoking1 on
// X). What already ends in /v1, what isn't Anthropic's, and a model whose
// own URL says no /v1 go as they did.
func TestPluginAnthropicBaseKeepsV1(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	type request struct {
		path, key string
	}
	calls := make(chan request, 16)
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- request{r.URL.Path, r.Header.Get("X-Api-Key")}
		w.WriteHeader(http.StatusNoContent)
	}))
	p := destinationProvider(t, ctx, vendor.URL+"/fallback", destinationSignIn("fixture-key", vendor.URL))
	for _, c := range []struct {
		name, model, base, path, want, key string
	}{
		{"anthropic bare loader", "claude", vendor.URL, "/v1/messages", "/v1/messages", "fixture-key"},
		{"loader trailing slash", "claude", vendor.URL + "/", "/v1/messages", "/v1/messages", "fixture-key"},
		{"loader authority with path", "claude", vendor.URL + "/enterprise", "/v1/messages", "/enterprise/v1/messages", "fixture-key"},
		{"chat keeps its path", "chat", vendor.URL, "/v1/chat/completions", "/chat/completions", ""},
		{"loader already ends in v1", "claude", vendor.URL + "/v1/", "/v1/messages", "/v1/messages", "fixture-key"},
		{"model URL when loader says none", "claude", "", "/v1/messages", "/model/v1/messages", "fixture-key"},
		{"model URL without v1", "roots", vendor.URL + "/native", "/v1/messages", "/native/messages", "fixture-key"},
		{"nested loader retains its v1", "claude", vendor.URL + "/api/v1/llm/v1", "/v1/messages", "/api/v1/llm/v1/messages", "fixture-key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := plugin.Restore(ctx, destinationPlugin, destinationPlugin, destinationSignIn("fixture-key", c.base)); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"model": c.model, "messages": []map[string]string{{"role": "user", "content": "fixture"}}})
			if err != nil {
				t.Fatal(err)
			}
			status, err := doDestinationRequest(ctx, p, pluginBase+destinationPlugin+c.path, "", string(body))
			if err != nil || status != http.StatusNoContent {
				t.Fatalf("Provider.Do = %d, %v, want 204", status, err)
			}
			select {
			case got := <-calls:
				if got.path != c.want || got.key != c.key {
					t.Errorf("vendor received %+v, want %s with key %q", got, c.want, c.key)
				}
			default:
				t.Error("response arrived without a vendor request")
			}
		})
	}
}
