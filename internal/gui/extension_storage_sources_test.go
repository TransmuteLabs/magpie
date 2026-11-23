package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStorageOwnerSelectionRejectsTrailingAndUnknownJSON(t *testing.T) {
	sandboxHome(t)
	mux := http.NewServeMux()
	extensionStorageSourceRoutes(mux)
	for _, path := range []string{"/api/sessions/storage-sources/bind", "/api/sessions/storage-sources/unbind"} {
		for _, body := range []string{`{} {}`, `{} garbage`, `{"rawPath":"/etc/passwd"}`, `{`, strings.Repeat(" ", 8193) + `{}`} {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatal("storage owner selection admitted malformed business input", path, response.Code, body[:min(len(body), 100)])
			}
		}
	}
}

func TestStorageOwnerRoutesKeepOrdinaryWebAuthenticationAndOrigin(t *testing.T) {
	sandboxHome(t)
	handler := webHandler("storage-proof", "fixture-key", "", nil, Handler(nil, nil))
	for _, path := range []string{"/api/sessions/storage-sources", "/api/sessions/storage-media?agent=opencode&session=selected&media=blob"} {
		request := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatal("storage owner read bypassed ordinary web sign-in", path, response.Code)
		}
		request = httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		request.AddCookie(&http.Cookie{Name: "magpie_web_storage-proof", Value: "fixture-key"})
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatal("storage owner read crossed the page origin guard", path, response.Code)
		}
	}
}
