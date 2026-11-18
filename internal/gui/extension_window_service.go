package gui

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func serveExtensionWindowService(w http.ResponseWriter, r *http.Request, part *extensionWindowPart, rel string) {
	binding, resource, ok := strings.Cut(strings.TrimPrefix(rel, pageServicePath), "/")
	if !ok || binding == "" {
		http.NotFound(w, r)
		return
	}
	for _, segment := range strings.Split(resource, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			http.Error(w, "invalid module service path", http.StatusBadRequest)
			return
		}
	}
	// Refuse foreign origins before acquiring or lazily starting a service.
	if origins, sent := r.Header["Origin"]; sent && (len(origins) != 1 || origins[0] != "null") {
		http.Error(w, "this is a plugin page's service: only its page asks it", http.StatusForbidden)
		return
	}
	endpoint, release, err := extensionWindows().ReadyWindowServiceTarget(r.Context(), part.id, part.revision, part.part, binding)
	if release != nil {
		defer release() // includes the entire HTTP/SSE/WebSocket response
	}
	if err != nil {
		fail(w, err)
		return
	}
	target, err := url.Parse(endpoint.URL)
	if err != nil || !windowServiceTarget(target) {
		http.Error(w, "invalid module service endpoint", http.StatusBadGateway)
		return
	}
	root := pluginPagePrefix + part.ref + "/" + pageServicePath + binding + "/"
	servePageServiceTarget(w, r, part.id, target, root, &endpoint)
}

// Readiness establishes RPC authority, while these response headers bind the
// actual HTTP peer to that same component instance and configuration.
func verifyWindowServiceProof(response *http.Response, target *extension.WindowServiceTarget) error {
	headers := response.Header
	for name, expected := range map[string]string{
		sdk.HeaderComponentInstance: target.Instance,
		sdk.HeaderComponent:         target.Component,
		sdk.HeaderComponentConfig:   target.ConfigVersion,
	} {
		values := headers.Values(name)
		if expected == "" || len(values) != 1 || values[0] != expected {
			return errors.New("module service generation proof does not match")
		}
	}
	stripWindowServiceProof(headers)
	stripWindowServiceProof(response.Trailer)
	if response.StatusCode != http.StatusSwitchingProtocols {
		// ReverseProxy copies trailers after reading and closing the body.
		// Leave an upgraded connection's duplex body intact.
		response.Body = &windowServiceProofBody{ReadCloser: response.Body, response: response}
	}
	return nil
}

type windowServiceProofBody struct {
	io.ReadCloser
	response *http.Response
}

func (body *windowServiceProofBody) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if err != nil {
		stripWindowServiceProof(body.response.Trailer)
	}
	return n, err
}

func (body *windowServiceProofBody) Close() error {
	err := body.ReadCloser.Close()
	stripWindowServiceProof(body.response.Trailer)
	return err
}

func stripWindowServiceProof(headers http.Header) {
	headers.Del(sdk.HeaderComponentInstance)
	headers.Del(sdk.HeaderComponent)
	headers.Del(sdk.HeaderComponentConfig)
}

func windowServiceTarget(target *url.URL) bool {
	if target == nil || target.Scheme != "http" && target.Scheme != "https" || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return false
	}
	ip := net.ParseIP(target.Hostname())
	port, err := strconv.Atoi(target.Port())
	return ip != nil && ip.IsLoopback() && err == nil && port > 0 && port <= 65535 && strconv.Itoa(port) != gateway.Port()
}

func windowServiceLocation(loc string, endpoint *url.URL) string {
	u, err := url.Parse(loc)
	if err != nil || u.Host != "" && u.Host != endpoint.Host || u.Scheme != "" && u.Scheme != endpoint.Scheme {
		return loc
	}
	base := strings.TrimSuffix(endpoint.Path, "/")
	if u.Path == base || strings.HasPrefix(u.Path, base+"/") {
		u.Path = strings.TrimPrefix(u.Path, base)
		if u.Path == "" {
			u.Path = "/"
		}
		u.RawPath = ""
		// serviceLocation already mediates root-relative paths, including
		// those on IPv6 loopback that its legacy port matcher doesn't name.
		u.Host, u.Scheme = "", ""
		return u.String()
	}
	return loc
}
