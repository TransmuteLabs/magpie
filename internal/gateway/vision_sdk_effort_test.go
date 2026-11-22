package gateway

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKVisionDescriptionEffort(t *testing.T) {
	const (
		eye         = "fixture-effort-eye"
		eyeWithNone = "fixture-effort-eye-with-none"
		eyeUnknown  = "fixture-effort-eye-unknown"
		description = "The SDK describes a yellow square."
	)
	type descriptionRequest struct {
		Account  string
		Model    string
		Protocol string
		Body     []byte
	}
	var requestsMu sync.Mutex
	var requestCount int
	var latest descriptionRequest
	providerID := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var request descriptionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requestCount++
		latest = request
		requestsMu.Unlock()
		io.WriteString(w, `{"chatAnswer":"`+description+`"}`)
	}, func(manifest *sdk.Manifest) {
		manifest.Contract = sdk.Contract{
			Version:      sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.AccountAuth, sdk.ModelDiscovery, sdk.OutboundExecutor},
		}
		manifest.Providers[0].Models = []sdk.Model{
			{ID: eye, Vision: true, Efforts: []string{"high", "low"}, Price: &sdk.ModelPrice{}},
			{ID: eyeWithNone, Vision: true, Efforts: []string{"high", "none", "low"}, Price: &sdk.ModelPrice{Input: 1}},
			{ID: eyeUnknown, Vision: true, Price: &sdk.ModelPrice{Input: 2}},
		}
	})
	text := &eyeUp{}
	textUpstream := httptest.NewServer(withoutAnonymousFixtureProbe(text))
	t.Cleanup(textUpstream.Close)
	base := textUpstream.URL + "/v1"
	if err := provider.Save(provider.Provider{ID: "probe", Name: "Probe", Chat: base, Key: "fixture", Models: []string{"text"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("probe", base, []catalog.Model{{ID: "text", ImageInput: imageInputBool(false)}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	for _, test := range []struct {
		name, model, pick, wantEffort string
		automatic                     bool
		wantMaxTokens                 int
	}{
		{name: "automatic minimum", model: eye, automatic: true, wantEffort: "low", wantMaxTokens: 4096},
		{name: "explicit tie favors higher effort", model: eye, pick: "medium", wantEffort: "high", wantMaxTokens: 28096},
		{name: "default turns thinking off", model: eyeWithNone, wantEffort: "none", wantMaxTokens: 4096},
		{name: "unknown levels leave effort out", model: eyeUnknown, pick: "high", wantMaxTokens: 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := settings.Load()
			current.Vision = ""
			if !test.automatic {
				current.Vision = providerID + "/" + test.model
			}
			current.VisionEffort = test.pick
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			requestsMu.Lock()
			before := requestCount
			requestsMu.Unlock()
			textBefore := len(text.sent("text"))
			image := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(test.name))
			body := `{"model":"probe/text","input":[{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_image","image_url":"` + image + `"}]}]}`
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "127.0.0.1:12345"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("description request: status %d, body %s", response.Code, response.Body.String())
			}
			requestsMu.Lock()
			count, sent := requestCount, latest
			requestsMu.Unlock()
			if count != before+1 || sent.Model != test.model || sent.Protocol != "chat" ||
				(sent.Account != "alice" && sent.Account != "bob") || !strings.Contains(string(sent.Body), image) {
				t.Fatalf("SDK description: %d requests, model %q, protocol %q, account %q, body %s", count-before, sent.Model, sent.Protocol, sent.Account, sent.Body)
			}
			var wire struct {
				Effort    *string `json:"reasoning_effort"`
				MaxTokens int     `json:"max_tokens"`
			}
			if err := json.Unmarshal(sent.Body, &wire); err != nil {
				t.Fatal(err)
			}
			effort := ""
			if wire.Effort != nil {
				effort = *wire.Effort
			}
			if effort != test.wantEffort || (wire.Effort == nil) != (test.wantEffort == "") || wire.MaxTokens != test.wantMaxTokens {
				t.Fatalf("SDK body %s, want effort %q and max_tokens %d", sent.Body, test.wantEffort, test.wantMaxTokens)
			}
			if got := text.sent("text"); len(got) != textBefore+1 || !strings.Contains(got[textBefore], description) ||
				strings.Contains(got[textBefore], image) {
				t.Fatalf("text model did not receive the description in place of the image: %v", got)
			}
		})
	}
}
