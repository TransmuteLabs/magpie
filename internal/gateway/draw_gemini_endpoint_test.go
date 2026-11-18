package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestGeminiDrawingKeepsSelectedBaseAndNamespacedModel(t *testing.T) {
	for _, mode := range []string{"native", "dual", "chat-only"} {
		for _, model := range []string{"organization/gemini-3-pro-image-preview", "models/organization/gemini-3-pro-image-preview"} {
			t.Run(mode+"/"+model, func(t *testing.T) {
				fresh(t)
				observed := make(chan string, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					observed <- request.URL.RequestURI()
					writer.Header().Set("Content-Type", "application/json")
					io.WriteString(writer, strings.TrimPrefix(drawnGemini, "data: "))
				}))
				t.Cleanup(upstream.Close)
				selected := provider.Provider{ID: "drawing", Name: "Drawing", Key: "fixture"}
				expectedBase := "/native/v1beta"
				if mode != "chat-only" {
					selected.Gemini = upstream.URL + expectedBase
				}
				if mode == "dual" {
					selected.Chat = upstream.URL + "/chat/v1"
				} else if mode == "chat-only" {
					selected.Chat = upstream.URL + "/v1beta/openai"
					expectedBase = "/v1beta"
				}
				server := New()
				t.Cleanup(server.idle.stop)
				reply, status, err := server.drawGemini(t.Context(), selected, model, drawing{Model: model, Prompt: "Draw a fixture", N: 1})
				if err != nil || status != http.StatusOK || len(reply.Images) != 1 {
					t.Fatalf("drawing failed: status=%d images=%d err=%v", status, len(reply.Images), err)
				}
				expected := expectedBase + "/models/organization/gemini-3-pro-image-preview:generateContent"
				if actual := <-observed; actual != expected {
					t.Fatalf("drawing reached %q, want selected Gemini endpoint %q", actual, expected)
				}
			})
		}
	}
}
