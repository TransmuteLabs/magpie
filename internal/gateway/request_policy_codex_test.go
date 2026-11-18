package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func TestSharedPolicyCodexOwnModelUsesSelectedLimits(t *testing.T) {
	fresh(t)
	scaled(t, time.Second)
	var sends atomic.Int64
	chatgpt(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		sends.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		writer.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
	})
	zero := 0
	current := settings.Load()
	current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{
		Models: map[string]requestpolicy.Options{"gpt-5.5": {MaxRetries: &zero}},
	}}
	if err := settings.Save(current); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, CodexPath+"/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":[]}`)).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer fixture-token")
	request.Header.Set("chatgpt-account-id", "fixture-account")
	response := httptest.NewRecorder()
	server := New()
	t.Cleanup(server.idle.stop)
	server.Front().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || sends.Load() != 1 {
		t.Fatalf("Codex own-model limit bypassed: status=%d sends=%d", response.Code, sends.Load())
	}
}
