package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtensionWindowHistoryHelloIsScoped(t *testing.T) {
	pagePlugins(t)
	_, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	mux := http.NewServeMux()
	pluginPageRoutes(mux, false)
	handler := ownWindow(mux)
	pages, _ := windowTestPages(t, handler)
	var ref string
	for _, page := range pages {
		if page.Ref != "" {
			ref = page.Ref
		}
	}
	if ref == "" {
		t.Fatal("fixture window missing")
	}
	for _, test := range []struct {
		body string
		code int
	}{
		{`{"ref":"` + ref + `","operation":"hello"}`, 200},
		{`{"ref":"` + ref + `","operation":"hello","account":"foreign"}`, 400},
		{`{"ref":"` + ref + `","operation":"hello"} {}`, 400},
		{`{"ref":"` + strings.Repeat("0", 32) + `","operation":"hello"}`, 409},
		{`{"ref":"` + ref + `","operation":"inspect","source":"foreign"}`, 409},
	} {
		request := httptest.NewRequest("POST", "/api/plugin-pages/history", strings.NewReader(test.body))
		response := do(handler, request)
		if response.Code != test.code {
			t.Fatalf("history request %s: want %d, got %d %s", test.body, test.code, response.Code, response.Body)
		}
		if test.code == 200 && !strings.Contains(response.Body.String(), `"features":[]`) {
			t.Fatalf("a window without a declared storage source negotiated history: %s", response.Body)
		}
	}
}
