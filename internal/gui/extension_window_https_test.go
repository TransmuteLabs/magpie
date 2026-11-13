package gui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionWindowServiceHTTPS(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	pages, _, err := extensionWindowParts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	part := pages[0].window
	proof := extension.WindowServiceTarget{Instance: "tls-instance", Component: "sidecar", ConfigVersion: part.config.Version}
	reserved := []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig}
	var serviceURL string
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("SDK service received a plaintext request")
		}
		for _, name := range reserved {
			if len(r.Header.Values(name)) != 0 {
				t.Errorf("browser proof forwarded to HTTPS backend: %s", name)
			}
		}
		module := sdk.ModuleContext{Instance: proof.Instance, Component: proof.Component, Config: sdk.ConfigSnapshot{Version: proof.ConfigVersion}}
		for name, values := range module.HTTPHeaders() {
			w.Header()[name] = values
		}
		if strings.HasSuffix(r.URL.Path, "/wrong-proof") {
			w.Header().Set(sdk.HeaderComponentInstance, "old-tls-instance")
		}
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			w.Header().Set("Location", serviceURL+strings.TrimSuffix(r.URL.Path, "redirect")+"read")
			w.WriteHeader(http.StatusFound)
		}
		_, _ = io.WriteString(w, "TLS_UPSTREAM_BODY")
	}))
	serviceURL = service.URL
	t.Cleanup(service.Close)
	original := pageServiceTransport
	trusted := original.Clone()
	trusted.TLSClientConfig = service.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if trusted.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("HTTPS test transport must verify the fixture certificate")
	}
	untrusted := original.Clone()
	untrusted.TLSClientConfig = nil // the normal system-root policy
	t.Cleanup(func() {
		pageServiceTransport = original
		trusted.CloseIdleConnections()
		untrusted.CloseIdleConnections()
	})
	var releases atomic.Int32
	endpointPath := "/"
	host.ready = func(ctx context.Context, id, revision, kind, binding string) (extension.WindowServiceTarget, func(), error) {
		if id != part.id || revision != part.revision || kind != part.part || binding != "page.service" {
			return extension.WindowServiceTarget{}, nil, errors.New("wrong HTTPS binding authority")
		}
		target := proof
		target.URL = service.URL + endpointPath
		return target, func() { releases.Add(1) }, nil
	}
	mux := http.NewServeMux()
	pluginPageRoutes(mux, false)
	request := func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", pluginPagePrefix+part.ref+"/"+pageServicePath+"page.service/"+path, nil)
		req.Header.Set("Origin", "null")
		for _, name := range reserved {
			req.Header.Set(name, "browser-proof")
		}
		before := releases.Load()
		w := do(mux, req)
		if releases.Load() != before+1 {
			t.Fatal("HTTPS service did not release its generation lease")
		}
		for _, name := range reserved {
			if len(w.Header().Values(name)) != 0 {
				t.Fatalf("HTTPS proof leaked to frontend: %s", name)
			}
		}
		return w
	}
	for _, tc := range []struct{ name, base string }{{"root", "/"}, {"prefixed", "/owned/"}} {
		t.Run(tc.name, func(t *testing.T) {
			pageServiceTransport = trusted
			endpointPath = tc.base
			w := request(t, "read")
			if w.Code != 200 || w.Body.String() != "TLS_UPSTREAM_BODY" {
				t.Fatalf("trusted HTTPS service rejected: %d %s", w.Code, w.Body)
			}
			w = request(t, "redirect")
			if w.Code != http.StatusFound || w.Header().Get("Location") != "read" {
				t.Fatalf("HTTPS redirect escaped the token proxy: %d %q", w.Code, w.Header().Get("Location"))
			}
			w = request(t, "wrong-proof")
			if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "TLS_UPSTREAM_BODY") {
				t.Fatalf("HTTPS bypassed generation proof: %d %s", w.Code, w.Body)
			}
		})
	}
	t.Run("untrusted certificate", func(t *testing.T) {
		pageServiceTransport = untrusted
		endpointPath = "/"
		w := request(t, "read")
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "TLS_UPSTREAM_BODY") {
			t.Fatalf("HTTPS certificate verification bypassed: %d %s", w.Code, w.Body)
		}
	})
}
