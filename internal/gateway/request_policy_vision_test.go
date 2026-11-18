package gateway

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func visionPolicyBody(images int) string {
	parts := make([]string, 0, images)
	for index := range images {
		image := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("policy screenshot %d", index)))
		parts = append(parts, `{"type":"input_image","image_url":"data:image/png;base64,`+image+`"}`)
	}
	return `{"model":"probe/text","input":[{"role":"user","content":[{"type":"input_text","text":"read"},` + strings.Join(parts, ",") + `]}]}`
}

func TestSharedPolicyIndependentVisionDescriptionsThroughFront(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			server, upstream := eyed(t)
			t.Cleanup(server.idle.stop)
			current := settings.Load()
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{MaxTotalRetries: &total}}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			const images = 12
			body := visionPolicyBody(images)
			for turn := 1; turn <= 2; turn++ {
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(t.Context())
				request.RemoteAddr = "127.0.0.1:12345"
				response := httptest.NewRecorder()
				server.Front().ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					var routes []string
					for _, route := range server.Trace(t.Context(), 0, 0).Routes {
						routes = append(routes, fmt.Sprintf("%s:%d:%s", route.Model, route.Status, route.Error))
					}
					t.Fatalf("turn=%d total=%d status=%d descriptions=%d text=%d body=%s routes=%v",
						turn, total, response.Code, len(upstream.sent("eye")), len(upstream.sent("text")), response.Body.String(), routes)
				}
			}
			if descriptions, texts := len(upstream.sent("eye")), len(upstream.sent("text")); descriptions != images || texts != 2 {
				t.Fatalf("independent first operations or cached descriptions changed: descriptions=%d text=%d", descriptions, texts)
			}
		})
	}
}
func TestSharedPolicyVisionFallbackSpendsRootBudget(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			server, first, second := twoEyed(t, "probe/eye")
			t.Cleanup(server.idle.stop)
			failing(first, true)
			zero := 0
			current := settings.Load()
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxTotalRetries: &total, MaxRetries: &zero, BaseDelayMS: &zero,
			}}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(visionPolicyBody(1))).WithContext(t.Context())
			request.RemoteAddr = "127.0.0.1:12345"
			response := httptest.NewRecorder()
			server.Front().ServeHTTP(response, request)
			wantStatus, wantSecond, wantText := http.StatusBadGateway, 0, 0
			if total == 1 {
				wantStatus, wantSecond, wantText = http.StatusOK, 1, 1
			}
			if response.Code != wantStatus || len(first.sent("eye")) != 1 || len(second.sent("bigeye")) != wantSecond || len(first.sent("text")) != wantText {
				t.Fatalf("total=%d status=%d first=%d fallback=%d text=%d body=%s",
					total, response.Code, len(first.sent("eye")), len(second.sent("bigeye")), len(first.sent("text")), response.Body.String())
			}
		})
	}
}

func TestSharedPolicyVisionRetriesShareParentBudget(t *testing.T) {
	for _, total := range []int{1, 2} {
		t.Run(fmt.Sprintf("total%d", total), func(t *testing.T) {
			fresh(t)
			busy := reply{http.StatusServiceUnavailable, "", `{"error":{"message":"temporarily unavailable"}}`}
			eye := &scripted{replies: []reply{busy, {200, "", chatOK}}}
			text := &scripted{replies: []reply{busy, {200, "", chatOK}}}
			var mutex sync.Mutex
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				request.Body = io.NopCloser(bytes.NewReader(body))
				mutex.Lock()
				defer mutex.Unlock()
				switch modelOf(body) {
				case "eye":
					eye.ServeHTTP(writer, request)
				case "text":
					text.ServeHTTP(writer, request)
				default:
					http.Error(writer, "unexpected model", http.StatusBadRequest)
				}
			}))
			t.Cleanup(upstream.Close)
			if err := provider.Save(provider.Provider{ID: "probe", Name: "Probe", Chat: upstream.URL + "/v1",
				Key: "key", Models: []string{"text", "eye"}, Patience: provider.PatienceOff}); err != nil {
				t.Fatal(err)
			}
			if err := catalog.SaveLive("probe", upstream.URL+"/v1", []catalog.Model{
				{ID: "text", ImageInput: imageInputBool(false)},
				{ID: "eye", Images: true, ImageInput: imageInputBool(true)},
			}); err != nil {
				t.Fatal(err)
			}
			zero, repeats := 0, 1
			current := settings.Load()
			current.Vision = "probe/eye"
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
				MaxTotalRetries: &total, MaxRetries: &repeats, BaseDelayMS: &zero,
			}}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			response := requestPolicyFront(t, pastedImage)
			mutex.Lock()
			eyeSends, textSends := eye.n, text.n
			mutex.Unlock()
			wantStatus := http.StatusServiceUnavailable
			if total == 2 {
				wantStatus = http.StatusOK
			}
			if response.Code != wantStatus || eyeSends != 2 || textSends != total {
				t.Fatalf("total=%d status=%d child=%d parent=%d body=%s", total, response.Code, eyeSends, textSends, response.Body.String())
			}
		})
	}
}
